# Cross-border Pay em Go — Documento de Escopo

> Projeto pessoal de aprendizado. **Não é** um gateway de pagamentos real, não processa
> dinheiro real, não tem clientes. É um clone simplificado do negócio do EBANX, com
> provedores simulados e, depois, adapters para sandboxes reais. Este documento define o
> escopo, as regras, a stack e as decisões fechadas antes de escrever código.

---

## 1. Visão geral e objetivo

Um lojista estrangeiro cobra em dólar; o pagador no Brasil paga em reais por PIX, boleto
ou cartão; o sistema converte, registra tudo num ledger de partidas dobradas e repassa ao
lojista em dólar.

**Obrigação inegociável:** o dinheiro registrado bate com o dinheiro que existe, sempre,
mesmo com falhas, duplicidade e eventos fora de ordem.

O objetivo não é o produto, é demonstrar quatro competências:

1. **Idempotência em toda a cadeia** — API, chamada ao provedor, eventos recebidos,
   webhooks enviados e jobs. Repetir nunca cobra, lança ou repassa duas vezes.
2. **Consistência do ledger sob concorrência** — milhares de operações simultâneas, com
   duplicatas, e o ledger fecha em zero por moeda.
3. **Escrita dupla sem transação distribuída** — o banco e o provedor nunca participam da
   mesma transação; intenção gravada antes, `unknown` em vez de falha, inbox e outbox.
4. **Tolerância a falhas do mundo externo** — provedores que atrasam, duplicam, reordenam
   e somem; worker que morre no meio; lojista fora do ar.

O exchange (Rust) provou correção em memória com determinismo total. Este projeto prova
correção **com banco, rede e falhas parciais**. Tudo que não servir a esses quatro eixos
fica pequeno.

**Critérios de sucesso**

- O teste de concorrência termina sempre com o ledger fechado em zero, nenhuma cobrança
  paga duas vezes e nenhum webhook perdido.
- Cada decisão da seção 11 está no README com a alternativa descartada e o motivo.
- O sistema está no ar com link público, dashboard e conta de demonstração (F10).
- Quem nunca viu o projeto entende o que ele faz em 30 segundos lendo o início do README.

---

## 2. Arquitetura — a decisão central

**Monólito modular, hexagonal** (decisões 1, 2, 3): dois binários do mesmo módulo Go, um
Postgres, nenhuma mensageria até a F10.

```
   lojista ──HTTP──> [ cmd/api ]  só recebe, valida e registra
                         │
                         │  Postgres (fonte da verdade + fila)
                         │  ledger · payments · idempotency_keys · inbox · outbox · jobs
                         │
   provedor ──HTTP──> [ cmd/api ]  webhook do provedor: grava na inbox, responde 200
                         │
                     [ cmd/worker ]  processa inbox, entrega outbox, resolve unknown,
                         │           expira, liquida, concilia (SKIP LOCKED)
                         v
                  provedores (simulador → sandbox real) · lojistas (webhooks)
```

A API só recebe e registra. Tudo que pode falhar ou demorar roda no worker, coordenado
pelo Postgres. Se a API passar a chamar coisas lentas dentro do request, ou se uma
chamada de rede entrar numa transação de banco, a corretude morre.

### 2.1 Módulos

Layout por módulo: `internal/<módulo>/{domain,app,adapters}`; `cmd/api` e `cmd/worker`
montam tudo.

| Módulo | Responsabilidade | Depende de |
|---|---|---|
| `merchants` | Lojista, API keys, URL de webhook | — |
| `payments` | Cobrança, tentativas, máquina de estados | `fx`, `ledger`, `providers` |
| `fx` | Cotação, travamento, conversão, arredondamento | — |
| `ledger` | Contas, transações contábeis, invariantes, saldos | — |
| `providers` | Porta comum + adapters (simuladores; sandboxes reais) | — |
| `events` | Inbox (entrada dos provedores) e outbox (saída para lojistas) | — |
| `webhooks` | Entrega, retry, HMAC, isolamento por lojista | `events`, `merchants` |
| `settlement` | Liquidação, estornos, chargebacks, reserva | `ledger`, `fx` |
| `reconciliation` | Conciliação, parser CNAB, fila de divergências | `ledger`, `providers` |
| `jobs` | Agendamento e execução assíncrona | — |
| `risk` (opcional) | Antifraude | `payments` |
| `routing` (opcional) | Escolha de adquirente | `providers` |

### 2.2 Regras de dependência

- O domínio não importa HTTP, SQL nem SDK de provedor. Isso vive em adapters.
- O ledger não conhece pagamentos: recebe transações contábeis prontas. Testável isolado
  e trocável por um motor próprio no futuro (decisão 10).
- Interfaces declaradas no pacote que consome, não no que fornece ("aceite interfaces,
  devolva structs").

---

## 3. Modelo de domínio e regras

### 3.1 Glossário

Nomes em inglês no código, banco e API.

| Termo (código) | Significado |
|---|---|
| `Merchant` | Empresa estrangeira que integra a API e recebe em dólar |
| `Payer` | Pessoa que paga em moeda local |
| `Provider` | Banco ou adquirente; simulado ou sandbox real |
| `Payment` | Intenção de cobrar um valor; tem ciclo de vida e várias tentativas |
| `PaymentAttempt` | Tentativa concreta num provedor: QR PIX, boleto, autorização de cartão |
| `Quote` | Taxa de câmbio travada por um período, ligada a uma cobrança |
| `Account` | Conta contábil interna do ledger, de uma moeda só |
| `Entry` | Débito ou crédito numa conta; nunca editado nem apagado |
| `Transaction` | Lançamentos que somam zero por moeda, aplicados atomicamente |
| `Settlement` | Repasse periódico do saldo do lojista, em dólar |
| `Refund` | Devolução pedida pelo lojista, total ou parcial |
| `Chargeback` | Contestação do banco do pagador, que retira o dinheiro depois |
| `IdempotencyKey` | Chave do cliente da API que torna a repetição segura |
| `Inbox` / `Outbox` | Eventos recebidos dos provedores / a enviar aos lojistas |
| `Reconciliation` | Conferência entre o que o provedor diz e o que o ledger registrou |
| minor units | Inteiro na menor unidade da moeda (centavos) |

Atores externos: **lojista** (API, webhooks, saldo), **pagador** (página de pagamento;
dados mínimos pela decisão 54),
**provedores** (falham, atrasam, duplicam e reordenam) e **operador interno** (filas de
revisão: antifraude, divergências, eventos inesperados).

### 3.2 Dinheiro e câmbio — regra inegociável

- **Nunca `float64`/`float32`** para dinheiro, taxa ou quantidade, nem no JSON de entrada.
- `Money{amount int64, currency Currency}` em minor units. Operação entre moedas
  diferentes retorna erro. Conversão só no módulo `fx`.
- **Taxa de câmbio** = inteiro escalado por 10^8 (`Rate`, 8 casas). Conversão:
  `destino = arredonda_half_even(origem × rate / 10^8)`, com produto intermediário sem
  overflow (`math/big` ou `math/bits`). Decisões 11 e 41.
- A diferença de arredondamento vai para a conta de arredondamento. A regra vive num
  único lugar do código.
- Taxa base vem de uma `RateSource` (fixa e configurável no início; PTAX como adapter
  depois). Ao pagador se aplica o spread (decisão 43).

### 3.3 Ledger e invariantes (o que o sistema nunca pode violar)

O ledger é a fonte da verdade financeira. Correções são novas transações, nunca edições.
Cada invariante vira teste (seção 9):

1. Em toda transação, **para cada moeda**, a soma dos lançamentos é zero.
2. Lançamentos nunca são alterados nem apagados (garantido também no banco, por
   permissão ou trigger).
3. Conta marcada "não pode ficar negativa" nunca fica negativa, mesmo sob concorrência.
4. Saldo em cache, se existir, é sempre igual à soma dos lançamentos da conta.
5. Uma conta tem exatamente uma moeda; lançamento em outra moeda é rejeitado.
6. A soma global de todas as contas, por moeda, é zero.

Invariantes de negócio que também viram teste:

7. Uma cobrança é paga no máximo uma vez (um único lançamento de confirmação).
8. A soma dos estornos de uma cobrança nunca passa do valor pago, mesmo com estornos
   simultâneos.
9. Todo evento da outbox é entregue pelo menos uma vez a quem tem endpoint ativo.

**Contas internas iniciais**

| Conta | Moeda | Para que serve |
|---|---|---|
| Recebimentos do provedor | BRL | Dinheiro confirmado, ainda não convertido |
| Câmbio (BRL) / Câmbio (USD) | BRL / USD | As duas pernas de cada conversão |
| Obrigação com lojista | USD | Quanto a empresa deve a cada lojista |
| Reserva de lojista | USD | Retenção para chargebacks (decisão 26) |
| Receita de taxas | USD | Taxa por transação (decisão 43) |
| Receita de spread | USD | Diferença entre câmbio base e cotado |
| Resultado cambial | USD | Variação absorvida em estornos (decisão 13) |
| Arredondamento | BRL / USD | Frações de centavo das conversões |
| Transitória de divergência | BRL | Dinheiro confirmado sem destino definido (seção 4.4) |
| Liquidações pagas | USD | Contrapartida dos repasses |

**Exemplo de referência (GameStore).** Jogo de US$ 20,00; taxa base 5,0000 BRL/USD;
spread 1,5% → cotada 5,0750; o pagador paga R$ 101,50. Taxa de 3,0% = US$ 0,60.
Lançamentos assinados da confirmação:

```
BRL:  Recebimentos do provedor  +10150
      Câmbio (BRL)              -10150      soma BRL = 0
USD:  Câmbio (USD)               -2030      (10150 / 5,0000)
      Obrigação com lojista      +1940
      Receita de taxas             +60
      Receita de spread            +30      soma USD = 0
```

### 3.4 Máquina de estados da cobrança

Toda mudança de status passa por uma tabela de transições e uma única função. Campos de
status não exportados; o linter `exhaustive` falha o build quando um `switch` esquece um
estado.

```
pending ──> paid ──> partially_refunded ──┐ (estornos parciais sucessivos)
   │          │            │   ^──────────┘
   │          │            ├──> refunded ──> charged_back   (decisão 7: abre disputa)
   │          │            └──> charged_back
   │          ├──> refunded
   │          └──> charged_back
   ├──> expired   [terminal; pagamento tardio segue a decisão 8]
   └──> canceled  [terminal]
```

| Estado | Significado | Sai para |
|---|---|---|
| `pending` | Criada, aguardando pagamento | `paid`, `expired`, `canceled` |
| `paid` | Confirmada e lançada no ledger | `partially_refunded`, `refunded`, `charged_back` |
| `partially_refunded` | Parte devolvida | `partially_refunded`, `refunded`, `charged_back` |
| `refunded` | Total devolvido | `charged_back` (com caso de disputa) |
| `charged_back` | Contestada pelo banco do pagador | terminal na v1 |
| `expired` | Cotação venceu sem pagamento | terminal |
| `canceled` | Cancelada pelo lojista antes do pagamento | terminal |

### 3.5 Estados da tentativa no provedor

| Estado | Significado |
|---|---|
| `created` | Intenção gravada, chamada ainda não feita |
| `sent` | Chamada em andamento |
| `unknown` | Timeout ou erro ambíguo: o provedor pode ter processado |
| `accepted` | Provedor confirmou a criação (QR gerado, cartão autorizado) |
| `rejected` | Recusa definitiva |
| `succeeded` | Dinheiro confirmado |

Testes obrigatórios das duas máquinas: cada estado tem entrada na tabela; transição fora
da tabela retorna `ErrInvalidTransition`; evento duplicado num estado que já o refletiu é
no-op idempotente, não erro.

---

## 4. Regras de negócio por subsistema

### 4.1 Fluxo ponta a ponta (roteiro GameStore)

Todo teste de ponta a ponta deriva deste roteiro.

1. **Criação.** `POST /v1/payments` com valor em USD, referência e `Idempotency-Key`. A
   key é registrada como "em processamento", o request é validado, a cobrança é criada.
2. **Cotação.** `fx` gera a cotação com validade (decisão 44) e calcula o BRL com spread
   e arredondamento.
3. **Tentativa.** `payments` grava a intenção, chama o provedor com o ID da tentativa
   como chave idempotente e recebe o BR Code. A resposta é guardada com a key.
4. **Repetição.** Repetição por timeout: a API acha a key, confere o hash do corpo e
   devolve a resposta guardada.
5. **Pagamento.** A confirmação do provedor é gravada na inbox (única pelo ID externo) e
   depois processada.
6. **Confirmação.** Numa única transação: cobrança → `paid`, transação contábil no ledger
   (seção 3.3) e `payment.paid` na outbox.
7. **Notificação.** O worker assina com HMAC e entrega; falhou, reagenda com backoff.
8. **Liquidação.** No corte diário, o saldo disponível vira repasse em USD.
9. **Depois.** Estorno ou chargeback geram novas transações, nunca edições.

Desvios cobertos: cotação expira sem pagamento (expira + evento); pagamento após a
expiração (decisão 8); confirmação duplicada (inbox descarta); timeout ao criar a
tentativa (`unknown`, decisão 17); lojista fora do ar (retry sem atrasar os outros).

### 4.2 Idempotência

Todo endpoint que cria ou muda dinheiro exige `Idempotency-Key`. Tabela
`idempotency_keys (merchant_id, key, request_hash, status, response_code, response_body,
locked_until, created_at)`, PK `(merchant_id, key)`. **O `INSERT` inicial é o lock.**

| Caso | Comportamento |
|---|---|
| Mesma key e corpo, primeira terminou | Devolve a resposta guardada (status e corpo) |
| Mesma key, corpo diferente | 422 `idempotency_key_reused` |
| Mesma key ao mesmo tempo | 409 imediato (decisão 15) |
| Primeira morreu no meio | Após `locked_until`, a key pode ser retomada |
| Primeira falhou com 4xx | Erro guardado e devolvido |
| Primeira falhou com 5xx antes do provedor | Key liberada (decisão 16) |
| Primeira falhou com 5xx depois do provedor | Key não liberada; tentativa `unknown` |
| Mesma key em lojistas diferentes | Independentes |

Validade de 24 h, com job de limpeza (decisão 14). Na cadeia: provedor recebe o ID da
tentativa como chave; inbox única por ID externo; cada webhook tem ID único; cada
liquidação é única por (lojista, data de corte).

### 4.3 Provedores e escrita dupla

- **Nenhuma chamada de rede dentro de transação de banco.**
- Timeout ou erro ambíguo → `unknown`. Recusa explícita → `rejected`.
- `unknown` nunca é repetida no escuro: primeiro se descobre o que aconteceu (decisão 17).
- Operações após o provedor responder usam contexto desacoplado do request: um cliente
  que desiste não desfaz localmente o que o provedor já fez.
- Todo adapter traduz para tipos do domínio; nada do provedor vaza.

```go
type Provider interface {
	CreateAttempt(ctx context.Context, req AttemptRequest) (AttemptResult, error)
	GetAttempt(ctx context.Context, id AttemptID) (AttemptResult, error)
	Refund(ctx context.Context, req RefundRequest) (RefundResult, error)
}
```

| Meio | Criação | Confirmação | Particularidades |
|---|---|---|---|
| PIX | BR Code (EMV + CRC16) com validade | Evento em segundos | Devolução é operação própria |
| Boleto | Linha digitável (47) e código de barras (44) | Retorno CNAB 240, dias depois | Vencimento em dias úteis; não recebe após vencer (decisão 53) |
| Cartão | Autorização | Captura | Autorização expira, captura parcial, chargeback |

**Simulador de provedor:** API que imita um provedor real (criar, consultar, estornar);
falhas injetáveis (atraso, timeout após processar, resposta duplicada, evento fora de
ordem, evento perdido); **controlado por seed**; endpoint de controle para testes e demo
("pagar esta cobrança agora").

### 4.4 Inbox: eventos fora de ordem e duplicados

O handler HTTP do provedor só grava em `inbox_events` (payload bruto, ID externo único,
status, tentativas) e responde 200; um worker processa (decisão 19).

| Situação | Ação |
|---|---|
| Novo e válido | Aplica a transição e lança no ledger |
| Duplicado | Constraint barra; no-op |
| Já refletido por outro caminho | No-op idempotente, registra a correlação |
| Antecipado (estorno antes da confirmação) | Fica pendente e é reprocessado quando a dependência chega; prazo vencido vira revisão |
| Divergência real (pós-expiração, valor diferente) | Dinheiro na conta transitória + caso de revisão |

Eventos de uma mesma cobrança são processados em série (lock na linha da cobrança).
**Dinheiro confirmado entra no ledger sempre; a dúvida é só para onde.**

### 4.5 Outbox e webhooks

Eventos para lojistas entram na outbox **na mesma transação do fato**. Garantia
at-least-once; o lojista deduplica pelo `id`.

```json
{
  "id": "evt_01J...",
  "type": "payment.paid",
  "created_at": "2026-11-03T14:21:07Z",
  "payment_id": "pay_01J...",
  "payment_version": 3,
  "data": { "status": "paid", "amount": { "value": 2000, "currency": "USD" } }
}
```

- `payment_version` cresce a cada transição; o lojista ignora versão menor que a última.
- Assinatura HMAC-SHA256 do corpo com timestamp no cabeçalho (rejeita replays antigos).
- Limite de entregas simultâneas por lojista; timeout curto; só 2xx é sucesso.
- **SSRF:** URL validada no cadastro e IP verificado após o DNS, bloqueando faixas
  privadas, loopback e metadados de cloud.
- Descoberta por polling `FOR UPDATE SKIP LOCKED` (decisão 22).
- Retry: backoff exponencial com jitter (decisão 23); destino que só falha é desativado
  com aviso (decisão 24); reentrega manual pela API.
- Limpeza periódica de eventos entregues.

### 4.6 Tempo

- O domínio **nunca** chama `time.Now()`: recebe um `Clock` (real e fake avançado
  manualmente nos testes). Tudo em UTC.
- Timestamps truncados para microssegundo na fronteira com o Postgres.
- Fuso local só em regras explícitas: corte de liquidação e vencimento de boleto em
  `America/Sao_Paulo`, configurável por país (decisão 20).
- Expirações rodam por job, mas **toda leitura também confere o prazo**.
- Dias úteis: calendário estático com feriados móveis calculados pela Páscoa (decisão 21).

### 4.7 Liquidação, estornos e chargebacks

- **Liquidação:** job diário por lojista, idempotente por (lojista, data de corte); só
  valores disponíveis (decisão 25); registro com a lista de pagamentos incluídos.
- **Estorno:** total ou parcial, nunca acima do pago (invariante 8); chama o provedor
  com as regras da seção 4.3; já liquidado sai do saldo futuro; câmbio pela decisão 13.
- **Chargeback:** chega por evento com motivo e valor; debita obrigação ou reserva, pode
  deixar o lojista negativo (decisão 27); gera evento e caso de disputa (aceitar ou
  contestar, resultado simulado).

### 4.8 Conciliação e CNAB

| Divergência | Tratamento |
|---|---|
| Faltando no sistema | Lançamento a partir da evidência ou caso |
| Faltando no provedor | Caso de revisão |
| Valor diferente | Caso com as duas versões |
| Status diferente | Resolve tentativas `unknown` ou abre caso |

- CNAB 240 (decisão 29): largura fixa, headers, segmentos T e U por título, trailers.
  Parser com tipos por registro, validação de contagens e totais, erro com número da
  linha. O simulador gera retornos no mesmo formato, com erros injetáveis.
- Boleto: código de barras de 44 posições, linha digitável de 47, DV módulo 10 e 11,
  fator de vencimento. **Conferir a especificação Febraban vigente e o manual de um banco
  antes de implementar.**
- Conciliação diária, com a lógica de comparação independente da origem (decisão 30).

### 4.9 API pública

REST + JSON, versionada em `/v1`, API key, valores em minor units, erros RFC 9457,
contrato OpenAPI escrito à mão (decisão 34).

| Método e caminho | Função |
|---|---|
| `POST /v1/payments` | Cria cobrança (`Idempotency-Key`) |
| `GET /v1/payments/{id}` | Cobrança e tentativas |
| `GET /v1/payments` | Lista com cursor e filtros |
| `POST /v1/payments/{id}/cancel` | Cancela antes do pagamento |
| `POST /v1/payments/{id}/capture` | Captura cartão autorizado |
| `POST /v1/payments/{id}/refunds` | Estorno (`Idempotency-Key`) |
| `GET /v1/balance` | Disponível, pendente e reservado |
| `GET /v1/settlements` | Liquidações e pagamentos incluídos |
| `GET /v1/events` | Eventos |
| `POST /v1/events/{id}/redeliver` | Reentrega de webhook |
| `PUT /v1/webhook-endpoint` | URL e rotação de segredo |

IDs opacos com prefixo (`pay_`, `att_`, `evt_`, `stl_`, `ref_`) sobre UUIDv7; paginação por
cursor; datas ISO 8601 UTC; request ID em toda resposta, ligado aos logs.

---

## 5. Stack técnico

| Camada | Escolha | Porquê |
|---|---|---|
| Linguagem | Go 1.27.3, fixado em `go.mod` (`toolchain`) e `mise.toml` | Concorrência, workers, deploy simples |
| Ferramentas e tarefas | `mise.toml`: versões de Go, `golangci-lint` 2.15 e `sqlc` 1.31 + tasks (`mise run test`) | Uma ferramenta fixa versões e roda tarefas (decisão 48) |
| HTTP | `net/http` + `ServeMux` da stdlib | Padrões de método e caminho bastam (decisão 35) |
| Banco | PostgreSQL 18 (`postgres:18.6-alpine` no compose e nos testes) | Transações, locks, `SKIP LOCKED`, `uuidv7()` nativo |
| Driver | `pgx/v5` + `pgxpool` | Controle de transação e tipos do Postgres |
| Acesso a dados | `sqlc`; SQL manual com `pgx` no ledger se preciso | SQL à mão, Go gerado (decisão 36) |
| Migrations | `goose` como biblioteca, SQL embutido por `embed`, aplicado por `cmd/migrate` e pelos testes | Mesma versão em dev, CI e produção (decisão 49) |
| IDs | UUIDv7 no banco, prefixo só na API | Ordenável, nativo (decisão 37) |
| Logs | `log/slog` em JSON | Stdlib |
| Testes | `testing` + `testcontainers-go`; `rapid` para propriedade | Postgres real nos testes |
| Contrato | OpenAPI à mão; tipos à mão; `kin-openapi` só nos testes de contrato | Contrato validado sem código gerado (decisão 50) |
| Lint e formato | `golangci-lint` v2 rigoroso + `gofumpt` (decisão 51) | Corretude por ferramenta |
| Config | Variáveis de ambiente lidas num struct no boot | Sem framework |
| Página do pagador e painel | `html/template` da stdlib, renderizado pelo `cmd/api`, sem framework JS | Frontend fica fora do escopo (decisão 55) |
| QR do PIX | BR Code à mão; imagem PNG por uma lib pequena, escolhida na F5 | O valor está no BR Code (decisão 56) |
| Observabilidade (F10) | OpenTelemetry, Prometheus, Grafana | — |
| Carga (F10) | k6 | — |

Dependência fora desta tabela só com pergunta e aprovação.

---

## 6. Segurança e modelo de ameaças

Fronteira de confiança: **adapters HTTP (API e webhook do provedor) são as únicas
superfícies com entrada não confiável; o domínio nunca vê bytes crus.**

- **Entrada malformada** → validação total no adapter; decimais no valor são rejeitados.
- **Criação de valor** → invariantes do ledger são propriedade de segurança.
- **Request repetido ou concorrente** → idempotência (seção 4.2).
- **Evento falso do provedor** → assinatura/mTLS do provedor no adapter real; inbox guarda
  a prova; conciliação acha o que não existe do outro lado.
- **SSRF via webhook** → seção 4.5.
- **Segredos** → API keys e segredos de webhook só como hash; segredo exibido uma vez;
  nada no repositório.
- **Dados pessoais** → logs sem CPF e e-mail (mascarados).
- **Exaustão** → rate limit por IP e por chave, limites por conta de demo, limpeza de
  tabelas que crescem.

### 6.1 Riscos previsíveis

| Risco | Sintoma | Prevenção |
|---|---|---|
| Precisão de tempo Go × Postgres | `time.Time` lido difere do gravado | Truncar em microssegundo |
| Pool esgotado | Latência enorme sem erro | Métricas do pool, timeout de aquisição |
| Rede dentro de transação | Provedor lento trava tudo | Regra fixa; revisão de PR procura |
| Deadlock no ledger | Erros de deadlock sob carga | Contas travadas em ordem crescente de ID |
| Retry parcial de transação | Inconsistência rara | `WithTx` repete a operação inteira |
| Contexto cancelado após o provedor | Rollback local com cobrança feita lá | Contexto desacoplado (seção 4.3) |
| Goroutines vazando | Teste não termina | Todo worker respeita `ctx.Done()`; teste de shutdown |
| Bug de concorrência raro | Passa 999 em 1000 | `-race`, `-count=100` no CI noturno, seed |
| Float no JSON | `20.099999` vira centavo errado | Inteiros na API |
| Crescimento sem limite | Keys, inbox e outbox enormes | Limpeza desde a F4 |
| Escopo infinito | Nunca termina | Seção 10 e "pronto quando" da seção 12 |
| Provedor real diferente do imaginado | Adapter quebra suposições | Revisar a porta ao integrar o primeiro sandbox |

---

## 7. Regulação e compliance

**Não sou advogado; o ponto principal é que a regulação não se aplica a um projeto de
estudo** com dinheiro simulado e sem clientes. Processar dinheiro de terceiros exigiria
autorização do Banco Central como instituição de pagamento. Fica fora e explicado no
README:

- **Impostos (IOF etc.)** → no máximo uma taxa configurável, com nota.
- **KYC e PLD/FT** → fora; trilha de auditoria do ledger e da inbox mostra o desenho.
- **PCI DSS** → nenhum número de cartão, só tokens fictícios.
- **LGPD** → minimização: não guardar o que não precisa; mascarar em logs.

---

## 8. Boa conduta

- Toda invariante coberta por teste; todo bug vira teste antes da correção.
- Cenário de falha reproduzível pela seed do simulador.
- Código auto-documentado; comentário só para comportamento não óbvio.
- Histórico de PRs que conta a evolução por fase.
- Licença MIT (decisão 42).
- **Honestidade no README:** simulador de aprendizado, dinheiro simulado, números
  ilustrativos documentados como tal.

---

## 9. Testes e garantia de corretude

| Camada | O que cobre | Ferramenta |
|---|---|---|
| Unitário | Estados, `Money`, arredondamento, BR Code, DVs, parser CNAB | `testing`, tabelas de casos |
| Propriedade | Invariantes do ledger com sequências aleatórias | `rapid` |
| Integração | Repositórios, transações, locks, inbox, outbox | `testcontainers-go` |
| Concorrência | Milhares de operações simultâneas com duplicatas | goroutines + Postgres real, `-race` |
| Falhas | Timeouts, duplicatas, reordenação, worker morto | simulador com seed |
| Contrato | Respostas batem com o OpenAPI | validador de OpenAPI |
| Ponta a ponta | Roteiro GameStore via HTTP | `testing` + compose |
| Carga (F10) | Throughput e p99 com gargalo identificado | k6 |

**Testes que o README cita** (a "métrica" do projeto, como o exchange cita os dele):

1. Mil confirmações simultâneas da mesma cobrança geram exatamente um lançamento.
2. Estornos parciais simultâneos nunca ultrapassam o valor pago.
3. Milhares de transações entre contas aleatórias: soma global zero, nenhuma conta
   proibida negativa.
4. Worker de webhooks morto no meio: após reinício, todo evento é entregue ao menos uma vez.
5. Mesma seed → mesmo histórico e mesmo estado final.

Números a reportar no README ao fim de cada fase relevante: casos do teste de
propriedade, operações do teste de concorrência e quantas rodadas `-count` passaram; na
F10, throughput e p50/p99 do `POST /v1/payments` e da confirmação, com o gargalo
identificado e a metodologia em `docs/benchmarks.md`.

Simulação determinística só no simulador de provedor; o Postgres é real (decisão 38).

---

## 10. Escopo: dentro e fora

**Dentro (ao longo das fases)**
- Cobrança, cotação, idempotência; ledger multi-moeda.
- PIX com BR Code real (pagável só no simulador); boleto com linha digitável real e
  compensação simulada via CNAB; cartão com tokens fictícios.
- Inbox, outbox, webhooks.
- Liquidação em USD, estornos, chargebacks, reserva.
- Conciliação com CNAB 240.
- Simulador hostil com seed; adapter Efí (mTLS), depois Asaas.
- Deploy público com dinheiro simulado, observabilidade, carga.

**Opcional (decisão 28):** roteamento de cartão, depois antifraude, segundo país; só
depois da F10.

**Fora**
- Dinheiro real de terceiros; impostos reais; KYC/PLD; armazenamento de cartão.
- Frontend elaborado: só a página do pagador e um painel mínimo.
- Mensageria (NATS, Kafka) antes de um motivo medido.
- Serviços separados antes da F10.
- Feriados municipais.

---

## 11. Decisões fechadas

Fechadas em 23/10/2026. Cada uma vai para o README com a alternativa descartada.

**Escopo e arquitetura**
1. **Monólito modular**: API e worker como dois binários do mesmo módulo Go. Descartado:
   serviços com mensageria desde o início (custo antes de saber onde dói).
2. **Layout por módulo**: `internal/<módulo>/{domain,app,adapters}`. Descartado: por
   camada (pacotes gigantes, módulos não extraíveis).
3. **Sem mensageria**: Postgres como fila (outbox e jobs com `SKIP LOCKED`). NATS só na
   F10 com motivo medido; Kafka descartado (pesado demais).
4. **Primeira entrega só PIX**, com a porta de provedor desenhada para cartão e boleto.
   Boleto por último (depende de CNAB e calendário).
5. **Só Brasil**, com país e moeda como dados, nunca `if` espalhado.

**Pagamentos e estados**
6. **`Payment` e `PaymentAttempt` separados**; uma cobrança tem várias tentativas.
7. **`refunded` aceita `charged_back`**, abrindo caso de disputa (o dinheiro sairia duas
   vezes).
8. **Pagamento após expiração**: devolver ao pagador por padrão, política configurável
   por lojista (honrar ou recotar). O dinheiro entra no ledger antes de qualquer decisão.

**Ledger e câmbio**
9. **Saldo derivado** (soma dos lançamentos); migrar para cache com a invariante 4 só
   depois de medir na carga.
10. **Ledger no Postgres** atrás de uma interface `Ledger`. Motor próprio é projeto
    futuro.
10b. **`READ COMMITTED` + `SELECT ... FOR UPDATE`** nas contas em ordem crescente de ID.
    `SERIALIZABLE` com retry entra num benchmark comparativo.
11. **Arredondamento half-even**, diferença na conta de arredondamento, regra num lugar só.
12. **Taxa fixa configurável** atrás de `RateSource`; PTAX como adapter depois (conferir
    termos e disponibilidade antes).
13. **Estorno parcial pela proporção do original**; a empresa absorve a variação na conta
    de resultado cambial.

**Idempotência e provedores**
14. **Key válida por 24 h**, com job de limpeza.
15. **Request concorrente com a mesma key → 409 imediato.**
16. **Guardar sucesso e 4xx; liberar a key em 5xx** — exceto se o provedor já foi chamado.
17. **`unknown` se resolve por consulta ativa** (`GetAttempt` com backoff), depois
    evento/conciliação como rede de segurança, revisão manual após limite de tempo.
18. **Cartão: captura automática por padrão**, captura separada suportada com expiração.
19. **Inbox como tabela própria**: handler grava e responde 200; worker processa.

**Tempo e eventos**
20. **Corte de liquidação em `America/Sao_Paulo`**, configurável por país.
21. **Calendário estático versionado**, móveis calculados pela Páscoa; sem dependência.
22. **Outbox por polling `SKIP LOCKED`**; `LISTEN/NOTIFY` + polling quando a latência for
    medida.
23. **Retry de webhook**: backoff exponencial com jitter total, base 30 s, fator 2, teto
    de 6 h por espera, desiste após 72 h desde a criação (~20 tentativas); depois
    `failed` com reentrega manual.
24. **Destino que só falha é desativado** com aviso ao lojista (circuit breaker por
    destino) após 72 h só de falhas.

**Pós-pagamento**
25. **Disponibilidade por meio, configurável**: PIX D+1, boleto D+2 após compensação,
    cartão D+30. Números ilustrativos.
26. **Reserva rolante** de chargeback (percentual retido por período), na F8.
27. **Saldo negativo**: consome a reserva, depois desconta das próximas liquidações;
    bloqueia novas cobranças acima de um limite de dívida.
28. **Módulos opcionais**: nenhum antes da F10; depois roteamento, depois antifraude.
29. **CNAB 240** (Febraban + manual de um banco grande); 400 só se sobrar tempo.
30. **Conciliação diária**, lógica de comparação independente da origem.

**API, stack e testes**
31. **Valores como inteiro em minor units** + moeda; decimais rejeitados com erro claro.
32. **Erros RFC 9457** com código estável (`idempotency_key_reused`, `quote_expired`).
33. **Chaves `sk_test_` / `sk_live_`**, modo teste sempre com simuladores, só hash
    guardado.
34. **Contrato primeiro**: OpenAPI escrito à mão.
35. **Roteador da stdlib.**
36. **`sqlc`** para CRUD e consultas fixas; SQL manual com `pgx` no ledger se o `sqlc`
    atrapalhar os locks.
37. **UUIDv7 no banco**, prefixo só na API.
38. **Simulação determinística só no simulador de provedor**, Postgres real. Descartado:
    simulação total como no exchange (perderia os bugs reais de lock e isolamento).

**Produção**
39. **Hospedagem: adiada para a F10.** Opções levantadas: VPS + Compose, AWS (ECS/EC2 +
    RDS), Fly.io/Railway. Decidir com preços e limites conferidos no dia. Premissa fixa:
    tudo roda em Docker Compose, então qualquer opção serve sem mudar o código.
40. **Sandbox real: Efí primeiro** (mTLS com `.p12`), **Asaas depois** (provar que trocar
    é configuração). Adapter sobre a API HTTP, sem SDK.

**Fechadas nesta sessão**
41. **Taxa de câmbio** como inteiro escalado por 10^8, produto intermediário sem overflow.
    Descartado: biblioteca decimal (dependência para algo pequeno e testável).
42. **Nome `cross-border-pay`, licença MIT**, como o exchange.
43. **Taxa de 3,0%** sobre o valor em USD, debitada da obrigação com o lojista; **spread
    de 1,5%** sobre a taxa base na cotação ao pagador. Números ilustrativos.
44. **Validade da cotação**: PIX 30 min; boleto até o vencimento (3 dias corridos); cartão
    usa a cotação do momento da autorização.
45. **Fases F0–F10** com critério de pronto (seção 12), agrupadas nas quatro fases do
    plano original.

**Repositório, ferramentas e regras finais**
46. **Repositório `github.com/gengibrepower/cross-border-pay`**, público, branch `master`,
    ruleset exigindo CI verde e squash merge, como o exchange.
47. **Idioma**: `README.md` em inglês + `README.pt-BR.md`; `docs/` e `AGENTS.md` em
    português; código, banco e API em inglês.
48. **`mise.toml`** fixa Go 1.27.3, `golangci-lint` 2.15 e `sqlc` 1.31 e guarda as tarefas
    (`fmt`, `lint`, `test`, `gen`, `db`, `migrate`). Descartados: Makefile, justfile.
49. **`goose` como biblioteca**, migrations SQL embutidas, aplicadas por `cmd/migrate` e
    pelos testes de integração. Descartado: CLI do goose (versão solta por máquina).
50. **OpenAPI à mão com tipos à mão**; `kin-openapi` valida cada request e response nos
    testes de contrato. Descartado: `oapi-codegen` (código gerado a explicar).
51. **Lint rigoroso**: padrão do `golangci-lint` + `exhaustive`, `gosec`, `errorlint`,
    `wrapcheck`, `gocritic`, `revive`, `bodyclose`, `sqlclosecheck`, `contextcheck`,
    `noctx`; formatador `gofumpt`. Tudo como erro no CI.
52. **Números operacionais** (ilustrativos, configuráveis): taxa base fixa 5,0000 BRL/USD;
    reserva de 10% de cada liquidação por 90 dias (decisão 26); bloqueio de novas
    cobranças com o lojista abaixo de US$ -1.000 (decisão 27); `GetAttempt` com base 5 s,
    fator 2, teto 5 min e revisão manual após 24 h (decisão 17); `locked_until` de 30 s
    na idempotência; evento antecipado espera até 24 h na inbox; webhook com timeout de
    10 s e até 4 entregas simultâneas por lojista; autorização de cartão expira em 7 dias.
53. **Boleto com "não receber após vencimento"**, sem multa nem juros: o banco simulado
    recusa depois do vencimento; um pagamento tardio que apareça no CNAB cai na decisão 8.
54. **Dados do pagador**: nome, CPF (com validação de dígitos) e e-mail opcional, todos
    mascarados em logs; a demo só aceita CPFs de teste gerados.
55. **Página do pagador e painel em `html/template`**, sem framework JS.
56. **QR do PIX**: BR Code (EMV + CRC16) à mão; a imagem sai de uma lib pequena e
    estável, escolhida na F5.
57. **CI**: em todo PR, formato, `vet`, lint, `go test -race` com cobertura reportada (sem
    meta) e `govulncheck`; job noturno com `-race -count=100`; Dependabot para módulos Go
    e actions.

---

## 12. Fases e definição de "pronto"

Cada fase só termina quando o critério é atingido. Uma fase vira **várias** PRs pequenas.

**Fase 1 — Núcleo de pagamentos (meta até a etapa presencial do EBANX)**

- **F0 — Esqueleto e CI**, em três PRs:
  1. `chore`: `go.mod`, `mise.toml`, layout da decisão 2, config por ambiente, `cmd/api`
     com `/healthz`, `cmd/worker` com shutdown gracioso.
  2. `ci`: `.golangci.yml` (decisão 51), GitHub Actions e Dependabot (decisão 57).
  3. `feat(db)`: `compose.yaml` com Postgres, goose embutido e `cmd/migrate`, helper de
     `testcontainers-go`, `WithTx`, `Clock`, `/readyz`. O `sqlc` fica para a F2, com as
     primeiras queries.

  *Pronto quando*: CI verde no PR; um teste de integração sobe Postgres, aplica as
  migrations e passa; `/healthz` responde; o worker para limpo com SIGTERM.
- **F1 — Dinheiro e câmbio**: `Money`, `Currency`, `Rate`, half-even, `RateSource` fixa,
  `Quote` com validade, spread e taxa. *Pronto quando*: tabelas de casos e teste de
  propriedade mostram que toda conversão fecha em zero contando a conta de
  arredondamento; operação entre moedas diferentes é erro.
- **F2 — Ledger**: contas, transações, invariantes 1–6, imutabilidade no banco, locks em
  ordem; `sqlc.yaml` e a task `gen` chegam com as primeiras queries. *Pronto quando*: teste de propriedade das invariantes e o teste citado 3 passam
  com `-race -count=20`.
- **F3 — Cobrança e API base**: lojistas e API keys (hash, prefixo), máquina de estados,
  OpenAPI à mão, `POST`/`GET /v1/payments`, RFC 9457. *Pronto quando*: toda transição
  fora da tabela retorna `ErrInvalidTransition` em teste exaustivo; respostas validadas
  contra o OpenAPI.
- **F4 — Idempotência**: tabela, os oito casos da seção 4.2, job de limpeza. *Pronto
  quando*: cada caso tem teste de integração, inclusive o concorrente (409).
- **F5 — PIX simulado e confirmação**: BR Code, simulador PIX com seed, tentativas,
  `unknown` resolvida por `GetAttempt`, inbox, confirmação transacional (cobrança +
  ledger + outbox), expiração. *Pronto quando*: teste citado 1 passa; BR Code validado
  contra exemplos da especificação do BC; cotação expirada não aceita pagamento em dia.
- **F6 — Outbox e webhooks**: worker de entrega, HMAC, retry da decisão 23, isolamento por
  lojista, SSRF, reentrega, decisão 24. *Pronto quando*: teste citado 4 passa e o roteiro
  GameStore roda ponta a ponta por HTTP; README com o fluxo e as decisões.

*Gate da Fase 1*: ledger fecha em zero sob concorrência. Corte mínimo se o prazo apertar:
F0–F5 + README; F6 logo depois.

**Fase 2 — Negócio completo**

- **F7 — Pós-pagamento**: cancelamento, estornos (decisão 13, invariante 8), liquidação
  diária em USD (decisões 20, 25), pagamento após expiração (decisão 8), `GET /balance`
  e `/settlements`. *Pronto quando*: teste citado 2 passa; liquidação rodada duas vezes
  no mesmo corte gera um repasse só.
- **F8 — Cartão, boleto e chargebacks**: autorização e captura (decisão 18), boleto com
  calendário (decisão 21), chargebacks com reserva e saldo negativo (decisões 7, 26, 27).
  *Pronto quando*: ciclo completo da GameStore, inclusive chargeback após liquidação,
  testado; nenhuma transição fora da tabela de estados.

**Fase 3 — Confiabilidade e integrações**

- **F9 — Simulador hostil, conciliação, sandbox real**: falhas injetáveis com seed, CNAB
  240 (parser e gerador), conciliação diária, adapter Efí com mTLS. *Pronto quando*:
  teste citado 5 passa; falhas injetadas não perdem nem duplicam dinheiro; o mesmo
  roteiro roda no sandbox Efí.

**Fase 4 — Produção e plataforma**

- **F10 — Produção**: hospedagem (decisão 39), HTTPS, backup com restauração testada,
  deploy por CI, migrations expand/contract, shutdown gracioso, health checks,
  observabilidade, k6, demo pública com limites. *Pronto quando*: sistema no ar com link,
  dashboard público, conta de demonstração e números de carga em `docs/benchmarks.md`.
