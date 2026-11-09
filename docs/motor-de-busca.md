# Motor de busca

[← Voltar ao índice](README.md)

Como um texto vira `has_profanity` + `censored_text` em microssegundos.

- [Visão geral](#visão-geral)
- [1. Normalização](#1-normalização)
- [2. A Trie](#2-a-trie)
- [3. Varredura](#3-varredura)
- [4. Censura no texto original](#4-censura-no-texto-original)
- [Exemplo completo, passo a passo](#exemplo-completo-passo-a-passo)
- [Complexidade](#complexidade)

---

## Visão geral

```mermaid
flowchart LR
    A["Texto original<br/>'Ola, P.U.T.A!'"] --> B["1. Normalização<br/>O(n), 1 passada"]
    B --> C["Texto canônico<br/>'ola puta'<br/>+ posição original de cada letra<br/>+ fronteiras"]
    C --> D["2–3. Varredura na Trie<br/>só em início de palavra"]
    D --> E["Match<br/>bytes [5, 12) do original"]
    E --> F["4. Censura<br/>no texto ORIGINAL"]
    F --> G["'Ola, *******!'"]
```

---

## 1. Normalização

Código: [internal/normalizer/normalizer.go](../internal/normalizer/normalizer.go)

Cada caractere do texto cai em **uma de quatro classes**:

```mermaid
flowchart TD
    R[caractere] --> Q{classe}
    Q -->|"letra, dígito,<br/>leetspeak"| L["LETRA<br/>vira minúscula, sem acento,<br/>leet → letra"]
    Q -->|"espaço, tab,<br/>quebra de linha"| S["ESPAÇO<br/>sequências viram um ' '<br/>(fronteira forte)"]
    Q -->|"pontuação,<br/>símbolos, emoji"| P["SEPARADOR<br/>removido, mas marca<br/>uma fronteira fraca"]
    Q -->|"acento combinante<br/>(U+0301…)"| X["DESCARTADO<br/>sem deixar rastro"]
```

| Regra | Entrada | Saída |
|---|---|---|
| Minúsculas | `MERDA` | `merda` |
| Leetspeak | `0 1 3 4 5 7 @ $` | `o i e a s t a s` |
| Acentos | `Desgraçado` | `desgracado` |
| Separadores | `p.u.t.a`, `c*a*r*a*l*h*o` | `puta`, `caralho` |
| Espaços | `"a   \t b"` | `"a b"` |

**Desempenho:** a classificação dos 384 primeiros caracteres Unicode (ASCII, Latin-1 e Latin Extended-A, que cobrem todo o português) é **pré-calculada numa tabela**. Classificar um caractere custa um acesso a array. Os demais passam pelo caminho lento (tabelas Unicode do Go), que continua correto.

### A normalização lembra de onde cada letra veio

Para censurar o texto **original** (mantendo acentos e pontuação das partes limpas), cada letra normalizada guarda o intervalo de bytes que ela ocupava:

```
original:     O  l  á  ,     P  .  U  .  T  .  A  !
bytes:        0  1  2-3 4 5  6  7  8  9  10 11 12 13
                 
normalizado:  o  l  a     ␣  p     u     t     a
Span:        [0,1)[1,2)[2,4) [5,6)[6,7) [8,9) [10,11) [12,13)
SoftBreak:                    ·      ✓     ✓      ✓
```

- `á` ocupa **2 bytes** em UTF-8, então o `a` normalizado aponta para `[2,4)`.
- `SoftBreak ✓` indica que um separador foi removido logo antes daquela letra.

### Fronteiras de palavra

Uma palavra só pode **começar e terminar** numa fronteira. É isso que separa `p.u.t.a` (detectado) de `computador` (não detectado):

```mermaid
flowchart LR
    subgraph "computador"
        direction LR
        c1[c] --- o1[o] --- m1[m] --- p1[p] --- u1[u] --- t1[t] --- a1[a] --- d1[d] --- o2[o] --- r1[r]
    end
    subgraph "p.u.t.a"
        direction LR
        p2[p] -.- u2[u] -.- t2[t] -.- a2[a]
    end
```

Em `computador` não há fronteira antes do `p` nem depois do `a`, então "puta" não pode casar ali. Em `p.u.t.a` os pontos viraram fronteiras fracas (linha tracejada), e a palavra inteira, de borda a borda, casa.

| Posição | É fronteira? |
|---|---|
| Início ou fim do texto | sim |
| Antes ou depois de um espaço | sim |
| Onde um separador foi removido (`.` `,` `!` `*` `-`…) | sim |
| Entre duas letras coladas | **não** |

---

## 2. A Trie

Código: [internal/trie/trie.go](../internal/trie/trie.go)

Cada nível da árvore é uma letra. Palavras com o mesmo começo **compartilham nós**, e cada nó onde uma palavra termina guarda uma **máscara de bits dos idiomas** a que ela pertence:

```mermaid
graph TD
    root((raiz))
    root --> p[p] --> o[o] --> r[r] --> r2[r] --> a1["a<br/>tags = 010<br/><b>porra</b> · pt-br"]
    p --> u[u] --> t[t] --> a2["a<br/>tags = 010<br/><b>puta</b> · pt-br"]
    t --> o3["o<br/>tags = 010<br/><b>puto</b> · pt-br"]
    root --> s[s] --> h[h] --> i[i] --> t2["t<br/>tags = 001<br/><b>shit</b> · en"]
    root --> v[v] --> a3[a] --> c[c] --> a4["a<br/>tags = 100<br/><b>vaca</b> · pt-br-strict"]
```

| Bit | Idioma | Opcional? |
|---|---|---|
| `001` (bit 0) | `en` | não |
| `010` (bit 1) | `pt-br` | não |
| `100` (bit 2) | `pt-br-strict` | sim |

Os bits seguem a ordem alfabética dos arquivos. O padrão do servidor (todos menos os opcionais) é `011`.

### Por que uma Trie só, e não uma por idioma

| | Uma Trie por idioma | **Uma Trie com máscara** |
|---|---|---|
| Requisição com 3 idiomas | Varre o texto 3 vezes e mescla os resultados | Varre **1 vez** |
| Filtrar por idioma | Escolher quais árvores percorrer | Um `AND` de bits por palavra candidata |
| Palavra em 2 idiomas | Duplicada | Um nó, 2 bits ligados |
| `detected_languages` | Juntar listas | `OR` das máscaras encontradas |

### Estrutura de um nó

```go
type node struct {
    children [27]*node      // 'a'..'z' + ' ': índice direto, sem hash
    extra    map[rune]*node // outros caracteres (dígitos, outros alfabetos)
    tags     Tags           // != 0 → uma palavra termina aqui
}
```

Depois da normalização, quase todo caractere é `a`–`z` ou espaço, e descer um nível é **um acesso a array**. O `map` só é criado nos raros nós que precisam dele.

---

## 3. Varredura

Código: [internal/filter/filter.go](../internal/filter/filter.go) → `scan` e `longestMatch`

```mermaid
flowchart TD
    S([i = 0]) --> A{"i < tamanho?"}
    A -->|não| FIM([fim: lista de matches])
    A -->|sim| B{"posição i é<br/>início de palavra?"}
    B -->|não| N1[i++] --> A
    B -->|sim| C["longestMatch(i):<br/>desce na Trie a partir de i"]
    C --> D{"achou palavra?"}
    D -->|não| N1
    D -->|sim| E["guarda o match<br/>i = fim do match"] --> A
```

### `longestMatch`: descer na Trie

```mermaid
flowchart TD
    S(["cursor = raiz<br/>melhor = nenhum"]) --> L{"próxima letra r"}
    L -->|acabou o texto| FIM([devolve o melhor])
    L --> A{"existe filho r?"}
    A -->|sim| M[cursor = filho]
    A -->|não| B{"r repete a letra<br/>anterior?"}
    B -->|"sim (porrrra)"| ST["fica no mesmo nó"]
    B -->|não| FIM
    M --> C{"tags do nó AND<br/>idiomas pedidos ≠ 0<br/>E próxima posição<br/>é fronteira?"}
    ST --> C
    C -->|sim| U["melhor = aqui<br/>(continua procurando<br/>um match mais longo)"] --> L
    C -->|não| L
```

Três regras importantes:

1. **Match mais longo:** em `filho da puta`, o algoritmo não para em `filho`. Ele continua e prefere a expressão inteira.
2. **Letras esticadas:** se a letra não tem aresta mas repete a anterior, o cursor fica parado. `porrrraaa` casa com `porra` sem cadastrar variações. Como a aresta real tem prioridade, `porra` (com `rr` legítimo) continua funcionando.
3. **Idioma é só um `AND`:** uma palavra que existe só num idioma não pedido simplesmente não conta como fim de palavra nesta requisição.

---

## 4. Censura no texto original

Os matches estão em posições do texto **normalizado**. Os `Span`s convertem para bytes do **original**, e cada caractere daquele trecho vira `*`, exceto espaços (para `filho da puta` → `***** ** ****`):

```
normalizado: o l a ␣ p u t a        match: índices 4..7
                     │     │
Span[4].Start = 5 ───┘     └─── Span[7].End = 12

original:  "Ola, P.U.T.A!"
                 ^^^^^^^        bytes [5, 12)
resultado: "Ola, *******!"      um * por caractere, inclusive os pontos
```

---

## Exemplo completo, passo a passo

Entrada: `"Que p0rrr4!"`, idiomas `pt-br`.

**1. Normalização**

| Original | `Q` | `u` | `e` | ` ` | `p` | `0` | `r` | `r` | `r` | `4` | `!` |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Classe | letra | letra | letra | espaço | letra | leet | letra | letra | letra | leet | separador |
| Normalizado | `q` | `u` | `e` | `␣` | `p` | `o` | `r` | `r` | `r` | `a` | — |

Resultado: `que porrra`. O `!` final foi removido e marca uma fronteira.

**2. Varredura**

| i | Início de palavra? | Descida na Trie | Resultado |
|---|---|---|---|
| 0 (`q`) | sim (início do texto) | `q`: sem filho | nada |
| 1–2 | não (letras coladas) | — | — |
| 3 (`␣`) | é espaço | — | — |
| 4 (`p`) | sim (depois de espaço) | `p → o → r → r`, depois `r` repetido: **fica**, depois `a` → tags `010`, fim do texto = fronteira | **match 4..9** |

**3. Censura:** posições 4..9 correspondem aos bytes `[4, 10)` do original, `p0rrr4`, que viram 6 asteriscos.

**Saída:** `"Que ******!"`

---

## Complexidade

| Etapa | Custo | Observação |
|---|---|---|
| Montar a Trie | O(soma dos tamanhos das palavras) | Uma vez, ~0,5 ms para ~400 palavras |
| Normalizar | O(n) | Uma passada, n = tamanho do texto |
| Varrer | O(n · L) | L = tamanho da maior palavra. **Não depende de quantas palavras existem.** |
| Censurar | O(n) | Uma passada |
| Memória por requisição | O(n) | Buffers reaproveitados via `sync.Pool`; texto limpo = 0 alocações |

Comparação com as alternativas:

| Abordagem | Custo por texto | De 10 para 10.000 palavras |
|---|---|---|
| `LIKE '%palavra%'` no banco | ida e volta na rede + O(W · n) | ~1000× mais trabalho (teórico) |
| Loop de `strings.Contains` | O(W · n) | ~1000× mais trabalho (teórico) |
| **Trie** | **O(n · L)** | **~12% mais lento** (efeito de cache, medido em benchmark) |
