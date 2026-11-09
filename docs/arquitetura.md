# Arquitetura

[← Voltar ao índice](README.md)

- [Pacotes](#pacotes)
- [Inicialização](#inicialização)
- [O caminho de uma requisição](#o-caminho-de-uma-requisição)
- [Concorrência: quem espera por quem](#concorrência-quem-espera-por-quem)
- [Desligamento](#desligamento)

---

## Pacotes

Cada pacote tem uma responsabilidade só, e as dependências apontam sempre "para baixo": o motor de busca (`trie`, `normalizer`) não sabe que existe HTTP.

```mermaid
flowchart TD
    main["cmd/server<br/><i>configuração, inicialização, shutdown</i>"]
    api["internal/api<br/><i>rotas HTTP, validação, JSON</i>"]
    mod["internal/moderation<br/><i>reclamações, quarentena, cadastro, persistência</i>"]
    filter["internal/filter<br/><i>idiomas, varredura, censura</i>"]
    trie["internal/trie<br/><i>árvore de prefixos</i>"]
    norm["internal/normalizer<br/><i>higienização do texto</i>"]
    wl["internal/wordlist<br/><i>leitura dos .txt</i>"]
    wls["wordlists<br/><i>.txt embutidos (go:embed)</i>"]
    clock["internal/clock<br/><i>cronômetro de alta resolução</i>"]

    main --> api
    main --> mod
    main --> wl
    main --> wls
    api --> mod
    api --> filter
    api --> clock
    mod --> filter
    mod --> norm
    mod --> wl
    filter --> trie
    filter --> norm
    filter --> wl
```

| Pacote | Responsabilidade | Arquivo principal |
|---|---|---|
| `cmd/server` | Lê variáveis de ambiente, monta tudo, sobe o servidor e desliga com segurança | [main.go](../cmd/server/main.go) |
| `internal/api` | Recebe HTTP, valida, chama o serviço e formata a resposta | [handler.go](../internal/api/handler.go), [moderation.go](../internal/api/moderation.go) |
| `internal/moderation` | Dono da Trie "viva": aplica decisões do admin e grava o estado | [moderation.go](../internal/moderation/moderation.go) |
| `internal/filter` | Junta normalização + Trie + idiomas e produz o texto censurado | [filter.go](../internal/filter/filter.go) |
| `internal/trie` | A árvore de prefixos com uma máscara de idiomas em cada palavra | [trie.go](../internal/trie/trie.go) |
| `internal/normalizer` | Converte o texto bruto na forma canônica, lembrando a posição original | [normalizer.go](../internal/normalizer/normalizer.go) |
| `internal/wordlist` | Lê um `<idioma>.txt` por idioma de qualquer `fs.FS` | [wordlist.go](../internal/wordlist/wordlist.go) |
| `internal/clock` | Mede microssegundos com precisão, inclusive no Windows | [clock.go](../internal/clock/clock.go) |

### Por que `cmd/` e `internal/`?

O projeto segue o layout padrão da comunidade Go:

| Pasta | Convenção | Aqui |
|---|---|---|
| `cmd/<nome>/` | Cada subpasta é **um executável**, e o nome dela vira o nome do binário | `cmd/server/` → `server.exe`. Uma ferramenta de linha de comando, por exemplo, entraria como `cmd/check/` reaproveitando o mesmo `internal/` |
| `internal/` | Código que **só este módulo pode importar**: o compilador Go bloqueia imports vindos de outros projetos | Todo o motor e a API. Nada aqui é uma biblioteca pública com compromisso de compatibilidade |
| `api/` | Contratos da API (OpenAPI, protobuf…) | `openapi.yaml` |
| `docs/` | Documentação | Estes arquivos |

Com isso, `main.go` fica pequeno: só lê a configuração e conecta as peças. Toda a lógica vive em pacotes testáveis dentro de `internal/`.

---

## Inicialização

Tudo que é caro acontece **uma vez**, antes de o servidor aceitar a primeira requisição. Erros de configuração derrubam o processo na hora (*fail fast*), em vez de aparecerem em produção.

```mermaid
sequenceDiagram
    autonumber
    participant M as main
    participant WL as wordlist
    participant MOD as moderation
    participant F as filter
    participant HTTP as http.Server

    M->>WL: LoadDir(embutido ou WORDLIST_DIR)
    WL-->>M: [en.txt, pt-br.txt, pt-br-strict.txt]
    M->>MOD: New(dicionários, STATE_FILE)
    MOD->>MOD: lê data/state.json (se existir)
    Note over MOD: arquivo corrompido = erro fatal
    MOD->>F: New(listas − palavras desativadas + palavras da API)
    F-->>MOD: Filter (Trie montada, ~0,5 ms)
    MOD->>MOD: current.Store(filter)
    M->>M: DEFAULT_LANGUAGES → LangSet
    Note over M: idioma inválido = erro fatal
    M->>M: ADMIN_TOKEN (vazio = rotas de admin desligadas)
    M->>HTTP: ListenAndServe(:PORT)
    M->>M: goroutine: grava reclamações a cada 5 s
```

| Variável | Padrão | Lida em |
|---|---|---|
| `PORT` | `8080` | criação do `http.Server` |
| `WORDLIST_DIR` | listas embutidas | `wordlist.LoadDir` |
| `STATE_FILE` | `data/state.json` | `moderation.New` |
| `DEFAULT_LANGUAGES` | todos menos `*-strict` | `filter.Resolve` |
| `ADMIN_TOKEN` | vazio (admin desligado) | `api.Config` |

---

## O caminho de uma requisição

Exemplo: `POST /v1/filter` com `{"text": "Que p0rr4!"}`.

```mermaid
sequenceDiagram
    autonumber
    participant C as Cliente
    participant R as recoverer
    participant H as handleFilter
    participant MOD as moderation
    participant F as Filter
    participant N as normalizer
    participant T as Trie

    C->>R: POST /v1/filter
    R->>H: (captura panics → 500)
    H->>H: lê JSON (limite 64 KiB)
    H->>MOD: Filter()
    MOD-->>H: snapshot atual (atomic.Load, sem lock)
    H->>H: languages → LangSet (ou padrão do servidor)
    rect rgba(120, 160, 255, 0.12)
    Note over H,T: trecho medido em elapsed_time
    H->>F: Censor(texto, LangSet)
    F->>F: pega buffer do sync.Pool
    F->>N: Normalize("Que p0rr4!")
    N-->>F: "que porra" + posição original de cada letra
    loop cada início de palavra
        F->>T: desce letra a letra
        T-->>F: palavra encontrada? de quais idiomas?
    end
    F->>F: troca as letras por * no texto ORIGINAL
    F->>F: devolve o buffer ao pool
    F-->>H: "Que *****!"
    end
    H-->>C: 200 JSON + Server-Timing
```

**O que é medido em `elapsed_time`:** só o algoritmo (normalizar, varrer, censurar). Ler o JSON e enviar a resposta pela rede ficam de fora, de propósito: assim o número reflete o motor, não a rede.

**Por que o texto limpo não aloca memória:** se nenhuma palavra é encontrada, `Censor` devolve a própria string de entrada, sem cópia. O buffer de normalização vem de um `sync.Pool` e volta para ele.

---

## Concorrência: quem espera por quem

O servidor atende muitas requisições ao mesmo tempo. O desenho garante que **filtrar texto nunca espera por nada**:

```mermaid
flowchart LR
    subgraph Leitores ["Leitores: nunca esperam"]
        R1[POST /v1/filter]
        R2[POST /v1/filter/batch]
        R3[GET /v1/languages]
    end
    subgraph Escritores ["Escritores: esperam só entre si"]
        W1[POST /v1/reports]
        W2[quarantine / restore / DELETE]
        W3[POST /v1/admin/words]
    end

    P{{"atomic.Pointer<br/>Filter atual"}}
    MU{{"sync.Mutex<br/>estado de moderação"}}

    R1 -->|Load| P
    R2 -->|Load| P
    R3 -->|Load| P
    W1 --> MU
    W2 --> MU
    W3 --> MU
    MU -->|"monta Trie nova<br/>e faz Store"| P
```

| Recurso | Proteção | Por quê |
|---|---|---|
| Trie em uso | Nenhuma: é imutável | Ninguém altera uma Trie publicada; mudanças criam outra |
| Ponteiro para a Trie atual | `atomic.Pointer` | Trocar o ponteiro é uma única instrução atômica |
| Mapas de moderação | `sync.Mutex` | Só as escritas (reclamações e admin) passam por ele |
| Buffers de normalização | `sync.Pool` | Cada requisição pega o seu; não há compartilhamento |

Detalhes da troca da Trie estão em [Moderação → Copy-on-write](moderacao.md#copy-on-write-trocando-a-trie-sem-parar-o-servidor).

---

## Desligamento

Ao receber `Ctrl+C` (`SIGINT`) ou `SIGTERM`:

```mermaid
sequenceDiagram
    participant OS as Sistema
    participant M as main
    participant HTTP as http.Server
    participant MOD as moderation

    OS->>M: SIGINT / SIGTERM
    M->>HTTP: Shutdown (até 10 s)
    Note over HTTP: para de aceitar conexões<br/>e termina as requisições em andamento
    HTTP-->>M: ok
    M->>MOD: Flush()
    Note over MOD: grava reclamações pendentes<br/>(as decisões do admin já estavam salvas)
    MOD-->>M: ok
    M->>OS: exit 0
```

Se o processo for **morto à força** (sem sinal), perdem-se no máximo os últimos 5 s de reclamações. Decisões do admin e palavras cadastradas nunca se perdem, porque são gravadas antes de a API responder.
