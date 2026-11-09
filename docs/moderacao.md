# Moderação

[← Voltar ao índice](README.md)

Como palavras são reclamadas, suspensas, removidas e cadastradas **com o servidor rodando**, sem deixar a filtragem mais lenta.

Código: [internal/moderation/moderation.go](../internal/moderation/moderation.go)

- [O fluxo completo](#o-fluxo-completo)
- [Estados de uma palavra](#estados-de-uma-palavra)
- [Um voto por cliente](#um-voto-por-cliente)
- [Copy-on-write: trocando a Trie sem parar o servidor](#copy-on-write-trocando-a-trie-sem-parar-o-servidor)
- [Persistência](#persistência)
- [Decisões de design](#decisões-de-design)

---

## O fluxo completo

```mermaid
sequenceDiagram
    autonumber
    actor U as Usuários
    participant API
    participant MOD as moderation
    actor A as Admin

    U->>API: POST /v1/reports {"word": "puto"}
    API->>MOD: Report(pt-br, puto, IP)
    MOD-->>API: reports: 1, counted: true
    API-->>U: 202 Accepted
    Note over U,MOD: ...outros usuários reclamam...

    A->>API: GET /v1/admin/reports
    API-->>A: puto: 12 reclamações, motivos
    A->>API: POST /v1/admin/words/pt-br/puto/quarantine
    API->>MOD: Quarantine(pt-br, puto)
    Note over MOD: monta Trie nova sem "puto",<br/>grava state.json, publica
    API-->>A: 200 status: quarantined
    Note over U,A: "fiquei puto" deixa de ser censurado imediatamente

    alt admin confirma que é palavrão
        A->>API: POST .../puto/restore
        Note over MOD: volta a censurar, reclamações encerradas
    else admin confirma que não é
        A->>API: DELETE .../puto
        Note over MOD: removida de vez, reclamações encerradas
    end
```

---

## Estados de uma palavra

```mermaid
stateDiagram-v2
    direction LR
    [*] --> active: carregada de um .txt
    [*] --> active: POST /v1/admin/words

    active --> quarantined: quarantine
    quarantined --> active: restore
    quarantined --> removed: DELETE
    active --> removed: DELETE
    removed --> active: restore

    removed --> removed: quarantine = 409

    state "active<br/>censura" as active
    state "quarantined<br/>não censura, em análise" as quarantined
    state "removed<br/>não censura, decisão final" as removed
```

| Estado | Censura? | Aceita reclamações? | Reclamações existentes |
|---|---|---|---|
| `active` | sim | sim | acumulando |
| `quarantined` | não | não (`409`) | **mantidas** como evidência para a análise |
| `removed` | não | não (`409`) | encerradas |

| Ação | O que acontece com as reclamações |
|---|---|
| `quarantine` | Mantidas |
| `restore` | Encerradas: o admin decidiu que **é** palavrão |
| `DELETE` | Encerradas: o admin decidiu que **não é** palavrão |

**Palavras cadastradas pela API** seguem o mesmo ciclo, com uma diferença: o `DELETE` as apaga por completo (não ficam como `removed`), porque não existe um `.txt` para onde "voltar". Elas podem ser cadastradas de novo depois.

---

## Um voto por cliente

Sem proteção, uma pessoa só poderia mandar 1000 reclamações e empurrar qualquer palavra para o topo da fila. Por isso cada cliente conta **uma vez por palavra**:

```mermaid
flowchart LR
    IP["IP do cliente<br/>203.0.113.7"] --> H["SHA-256(salt + ':' + IP)<br/>primeiros 16 bytes"]
    SALT[("salt aleatório<br/>gerado na 1ª execução,<br/>salvo no state.json")] --> H
    H --> ID["a91f…3c07"]
    ID --> Q{"já está na lista<br/>de quem reclamou<br/>desta palavra?"}
    Q -->|sim| N["counted: false"]
    Q -->|não| Y["counted: true<br/>reports + 1"]
```

- **Sem dados pessoais no disco:** o `state.json` guarda o hash, nunca o IP.
- **Por que *salt*:** existem só ~4 bilhões de IPv4, e um hash puro poderia ser revertido testando todos. Com um *salt* secreto, isso fica inviável.
- **Por que o *salt* é salvo:** se fosse gerado a cada execução, o mesmo IP teria outro hash depois de um reinício e poderia votar de novo.
- **Limites:** até 10.000 clientes distintos e os 10 motivos mais recentes por palavra, com até 280 caracteres cada.

> A quarentena **nunca é automática**. Se 500 reclamações liberassem uma palavra sozinhas, um grupo organizado poderia liberar qualquer palavrão. A fila só informa; quem decide é o admin.

---

## Copy-on-write: trocando a Trie sem parar o servidor

A Trie é lida por todas as requisições **sem nenhuma trava**, e é isso que garante latência de microssegundos. Alterá-la com o servidor rodando exigiria uma trava em toda leitura. A solução é nunca alterar: **montar uma nova e trocar o ponteiro**.

```mermaid
sequenceDiagram
    participant R1 as Requisição A
    participant R2 as Requisição B
    participant P as atomic.Pointer
    participant ADM as Admin (quarantine)
    participant D as Disco

    R1->>P: Load()
    P-->>R1: Trie v1
    Note over R1: filtrando com v1...

    ADM->>ADM: mutex.Lock()
    ADM->>ADM: monta Trie v2<br/>(listas − palavras desativadas)
    ADM->>D: grava state.json<br/>(temp + rename)
    ADM->>P: Store(v2)
    ADM->>ADM: mutex.Unlock()

    R2->>P: Load()
    P-->>R2: Trie v2
    Note over R1: ...A termina com v1,<br/>sem ser interrompida
    Note over R2: B já usa v2
    Note over P: v1 é liberada pelo<br/>garbage collector quando<br/>ninguém mais a usa
```

**A ordem importa: montar → gravar → publicar.**

```mermaid
flowchart LR
    A["1. Monta Trie nova<br/>(ainda invisível)"] --> B{"2. Grava<br/>state.json"}
    B -->|ok| C["3. Publica<br/>atomic.Store"]
    B -->|falhou| X["Desfaz a mudança na memória<br/>nada é publicado<br/>resposta 500"]
```

Se a gravação falhar, nada é publicado e a mudança em memória é desfeita. Assim, **memória, disco e Trie em uso nunca discordam**. Se a ordem fosse "publicar e depois gravar", uma falha de disco deixaria o servidor obedecendo a uma regra que sumiria no próximo reinício.

| | Mutex em toda leitura | **Copy-on-write** |
|---|---|---|
| Custo de uma leitura | lock + unlock | uma leitura atômica |
| Leitura durante uma mudança | espera | não espera |
| Custo de uma mudança | baixo | monta a Trie (~0,5 ms) |
| Adequado quando | escritas frequentes | **leituras ≫ escritas** (o caso aqui) |

Um teste ([moderation_test.go](../internal/moderation/moderation_test.go) → `TestReadersNeverSeeAPartialTrie`) alterna quarentena e restore 200 vezes enquanto 8 goroutines filtram texto sem parar, verificando que nenhuma leitura vê um estado intermediário.

---

## Persistência

Os `.txt` são a **base** e nunca são alterados. As mudanças ficam num arquivo separado, aplicado **por cima** da base na inicialização:

```mermaid
flowchart LR
    TXT["wordlists/*.txt<br/>(base, só leitura)"] --> M(("Trie<br/>ativa"))
    ADD["state.json → added<br/>(+ palavras da API)"] --> M
    DEC["state.json → words<br/>(− quarentena, − removidas)"] --> M
```

### Formato do `state.json`

```json
{
  "version": 1,
  "salt": "e7422118ba5de63494ea343c08641daa",
  "words": [
    { "language": "pt-br", "word": "puto", "status": "quarantined", "updated_at": "2026-11-09T00:23:02Z" }
  ],
  "added": [
    { "language": "pt-br", "word": "mané", "added_at": "2026-11-09T00:32:39Z" }
  ],
  "reports": [
    { "language": "pt-br", "word": "puto",
      "reporters": ["a91f0c...", "77be12..."],
      "reasons": ["em PT-PT puto = menino"],
      "first_reported_at": "2026-11-09T00:23:01Z", "last_reported_at": "2026-11-09T00:25:40Z" }
  ]
}
```

### Quando o arquivo é gravado

```mermaid
flowchart TD
    subgraph Imediato ["Gravação imediata, antes de responder"]
        A1[quarantine]
        A2[restore]
        A3[DELETE]
        A4[POST /v1/admin/words]
    end
    subgraph Agrupado ["Gravação agrupada"]
        R[POST /v1/reports] --> DIRTY["marca 'pendente'"]
        DIRTY --> T["a cada 5 s"]
        DIRTY --> SD["no desligamento"]
    end
```

- **Decisões do admin** são gravadas antes da resposta `200`. Se a API respondeu, a mudança está no disco.
- **Reclamações** são agrupadas: 1000 reclamações num segundo viram uma gravação, não 1000.

### Escrita atômica

```mermaid
sequenceDiagram
    participant S as Serviço
    participant FS as Disco

    S->>FS: cria data/.state-123.json (temporário)
    S->>FS: escreve o JSON
    S->>FS: Sync() (força ir para o disco)
    S->>FS: Rename(.state-123.json → state.json)
    Note over FS: o rename é atômico: quem abrir o arquivo<br/>vê a versão antiga OU a nova, nunca metade
```

Se o processo cair no meio da escrita, o `state.json` antigo continua intacto. Um arquivo corrompido (editado à mão, por exemplo) **impede o servidor de subir**, em vez de ser ignorado e apagar as decisões em silêncio.

---

## Decisões de design

| Decisão | Alternativa descartada | Motivo |
|---|---|---|
| Quarentena só manual | Automática após N reclamações | Reclamações em massa liberariam palavrões |
| 1 voto por IP (hash com salt) | Contar toda reclamação | Uma pessoa só não pode inflar a fila |
| Copy-on-write com `atomic.Pointer` | `sync.RWMutex` na Trie | Leituras são milhares de vezes mais frequentes que mudanças |
| Estado em arquivo separado | Editar os `.txt` | A base fica versionada no git; as decisões são dados de execução |
| Montar → gravar → publicar | Publicar → gravar | Uma falha de disco não pode deixar memória e disco divergentes |
| Recusar texto com encoding errado | "Consertar" removendo o caractere | `mané` corrompido viraria `man` e censuraria inglês |
| Normalizar palavras nas rotas | Comparar como foi digitado | `V4CA`, `vaca` e `váca` são a mesma palavra para o filtro |

**Limitação conhecida:** o estado é um arquivo local, ou seja, um por instância. Com vários servidores atrás de um *load balancer*, ele precisaria ir para um banco compartilhado (Postgres/Redis), com *pub/sub* avisando as instâncias para reconstruírem a Trie.
