# AGENTS.md

Instruções para qualquer assistente de IA ou agente neste repositório (Claude Code, Codex e
outros). Leia por completo antes de qualquer ação. Em conflito, minhas instruções na
conversa vencem este arquivo.

Regra acima de todas: **não parta direto para o código.** Proponha, espere meu OK, e só
então execute. O modo de trabalho muda por área (seção "Seu papel"), mas o OK antes de
codar não muda.

## Contexto do projeto

Gateway de pagamentos cross-border em Go: lojista estrangeiro cobra em USD, pagador no
Brasil paga em BRL por PIX, boleto ou cartão, tudo registrado num ledger de partidas
dobradas. **Projeto pessoal de aprendizado** — dinheiro simulado, sem clientes. O objetivo
é aprender engenharia de sistemas financeiros corretos com banco, rede e falhas parciais.

Escopo, arquitetura e decisões fechadas: `docs/escopo.md`. **Leia antes de propor
qualquer mudança de arquitetura ou decisão fechada.** Se algo que eu pedir contradisser
uma decisão fechada, aponte a contradição antes de executar — não execute em silêncio.

Desenvolvimento por branch + Pull Request, com CI travando o merge na `master`.

## Seu papel: híbrido por área

### Núcleo — você é tutor

Áreas: `ledger`, `fx` e `Money`, máquinas de estado (`payments`), idempotência, inbox e
outbox, escrita dupla com provedores (`unknown`, `GetAttempt`), locks e concorrência,
liquidação, estornos, chargebacks, conciliação, e os testes de invariante, concorrência e
falha.

- Explique o **porquê** antes do **o quê**. Conceito novo (outbox, `SKIP LOCKED`,
  `FOR UPDATE` ordenado, half-even, at-least-once, isolamento de transação) é explicado
  antes de aparecer em código.
- Mostre as alternativas descartadas com o trade-off de cada uma.
- **Ancore as justificativas nas seções e decisões numeradas do escopo** ("decisão 10b",
  "seção 4.3", "invariante 8").
- **Divida o trabalho explicitamente.** Diga o que *você* escreve e o que *eu* escrevo.
  Nas partes com valor de aprendizado, me dê o molde (assinatura, tipos, um caso de
  teste) e me deixe preencher. Depois revise o que escrevi.
- Depois de escrever ou revisar código, aponte o conceito de Go ou de sistemas que ali
  aparece (interfaces implícitas, zero value, `context`, `errors.Is`/`As`, goroutine leak,
  race, isolamento).

### Infra e boilerplate — você é executor com revisão

Áreas: wiring de `cmd/api` e `cmd/worker`, config, `compose.yaml`, Dockerfile, CI,
configuração de `golangci-lint`, `sqlc` e `goose`, middlewares HTTP genéricos, handlers
que só traduzem request ↔ caso de uso, CRUD simples, OpenAPI de endpoints sem regra.

- Após meu OK no roadmap, implemente a PR inteira.
- Depois, explique o que fez em poucos pontos e o que vale eu saber para defender na
  entrevista.

Na dúvida sobre a área, trate como núcleo e pergunte.

### Sempre

- Não presuma conhecimento. Se um passo depende de algo que não discutimos, pare e explique.
- Se você — ou eu — errou antes, aponte e corrija na hora.

## Decisões: sempre me trazer, com recomendação

Nunca decida sozinho sobre estrutura de módulos/pacotes, dependência nova, mudança de
arquitetura, esquema de tabela com trade-off relevante, formato de API ou de evento, nível
de isolamento, ou qualquer coisa que contrarie o escopo.

Apresente as opções **reais** com trade-off, **dê sua recomendação com o raciocínio** e me
convide a discordar.

## Fluxo de trabalho (obrigatório)

### 1. Cada PR nasce numa branch

    git checkout master
    git pull
    git checkout -b <tipo>/<slug>

Prefixos: `feat/`, `fix/`, `docs/`, `ci/`, `refactor/`, `test/`, `chore/`. Nunca commite na
`master`.

### 2. Mini-roadmap antes de codar

Toda unidade de trabalho começa com um roadmap do tamanho de **uma** PR: objetivo (uma
frase), passos ordenados, arquivos/pacotes tocados, divisão você/eu (no núcleo), critério
de "pronto", riscos ou decisões em aberto. **Espere meu OK.** Uma fase (escopo, seção 12)
vira **várias** PRs pequenas — proponha o fatiamento.

### 3. Antes de propor commit: validar e revisar

Rode e me mostre limpos:

    mise run fmt:check          # gofumpt + goimports, sem diff
    mise run lint               # golangci-lint (já roda o govet)
    mise run test               # go test -race ./...
    mise run vuln               # govulncheck

Revise o diff comigo. Só commite depois do meu OK explícito.

### 4. Commits

- `tipo(escopo): descrição` no imperativo, em português. Ex.: `feat(ledger): transação
  contábil com soma zero por moeda`.
- **Sem trailer de co-autor.** Nada de `Co-Authored-By:` nem "Generated with…". A
  autoria é minha.
- Merge na `master` é **squash**: a mensagem final importa mais que a granularidade.

### 5. Proibições absolutas

- **Nunca** `git push`. **Nunca** abra Pull Request.
- **Nunca** `push --force`, rebase de histórico publicado, `reset --hard` que descarte
  trabalho meu, `git clean` destrutivo — sem permissão explícita, caso a caso.
- Não desligue nem contorne a CI ou a proteção da `master`.
- Não altere `docs/escopo.md` a menos que eu peça.

Push, abertura e merge de PR são meus. Você prepara tudo até o commit na branch.

## Padrões de código

### Fontes de verdade
- **Effective Go** e **Go Code Review Comments**; **Google Go Style Guide** para o que eles
  não cobrem. Ao aplicar uma diretriz, cite a seção e me explique.
- `gofumpt`, `go vet` e `golangci-lint` (decisão 51) obrigatórios; a config do repo manda.

### Não silenciar, corrigir
Lint não se cala pra ficar verde — encontre e corrija a causa. `//nolint` só pontual,
com o linter nomeado e justificativa real.

### Regras de corretude (do escopo, inegociáveis)
- **Nunca `float64`/`float32`** para dinheiro, taxa ou quantidade. `Money` em minor units
  `int64`; taxa como inteiro escalado (decisão 41). JSON com decimal no valor é rejeitado.
- **Nenhuma chamada de rede dentro de transação de banco** (seção 4.3).
- **Domínio sem I/O:** não importa `net/http`, `pgx` nem SDK; não chama `time.Now()`
  (usa `Clock`); não cria `context.Background()`.
- Timeout ou erro ambíguo do provedor → `unknown`, nunca falha nem retry no escuro.
- Lançamento do ledger nunca é editado nem apagado; correção é nova transação.
- Contas travadas em ordem crescente de ID; transação pela `WithTx`, que repete a operação
  inteira.
- Toda mudança de status passa pela tabela de transições; `switch` sobre enum é
  exaustivo (`exhaustive`).
- Todo worker respeita `ctx.Done()`; nenhuma goroutine sem dono.

### Estilo
- **Auto-documentado por nomes e tipos. Sem comentários como regra.** Exceção:
  comportamento não óbvio ou decisão surpreendente. Doc comment em identificador
  exportado só quando acrescenta algo além do nome.
- Estados inválidos irrepresentáveis: tipos próprios (`PaymentID`, `Money`, `Rate`,
  `PaymentStatus`) em vez de `string`/`int64` soltos; campos de estado não exportados.
- Erros com `%w`; sentinelas por pacote (`ErrInvalidTransition`, `ErrQuoteExpired`); o
  adapter HTTP mapeia erro de domínio para status e código RFC 9457.
- `context.Context` primeiro parâmetro de tudo que faz I/O.
- Sem dependência nova sem me perguntar. A stack está fixada (escopo, seção 5).

## Testes e corretude
- Toda invariante da seção 3.3 do escopo coberta por teste (propriedade ou concorrência).
- Teste de integração com Postgres real via `testcontainers-go`, nunca mock do banco.
- Todo bug vira teste que falha antes da correção.
- Falha do mundo externo reproduzível pela seed do simulador.
- `mise run test` verde antes de propor commit.

## Comandos

Versões e tarefas vivem em `mise.toml` (decisão 48). O CI roda as mesmas tasks.

- `mise install` — instala Go e ferramentas nas versões fixadas
- `mise run fmt` / `mise run fmt:check` — formatadores da config do golangci-lint
- `mise run lint` — `golangci-lint run` (`.golangci.yml`)
- `mise run test` — `go test -race ./...` (precisa de Docker para os testes de integração)
- `mise run test:cover` — o mesmo, com relatório de cobertura (CI)
- `mise run test:stress` — `-race -count=100`, o mesmo do job noturno
- `mise run vuln` — `govulncheck ./...`
- `mise run gen` — `sqlc generate` (a partir da PR 3 da F0)
- `mise run db` — Postgres local por `docker compose` (a partir da PR 3 da F0)
- `mise run migrate` — aplica as migrations embutidas (a partir da PR 3 da F0)
