# Plano de polimento (F9) e novo escopo

Escrito em 14/11/2026, depois do merge da F8 (PR #56). Serve para retomar o trabalho em
sessões novas: cada sessão lê este arquivo, pega o próximo item não feito e atualiza o
registro de progresso no fim.

## 1. Mudança de escopo

O projeto serve à entrevista na EBANX. O que conta é o raciocínio sobre dinheiro, falhas e
consistência, que as fases F0–F8 já provam. Os cortes abaixo trocam operação e integração
por um código menor e mais fácil de ler.

| Antes | Agora | Motivo |
|---|---|---|
| F9: simulador hostil, CNAB 240, conciliação, sandbox Efí | **F9: polimento** (este plano) | O código está inchado; quem avalia lê código antes de rodar |
| Adapter Efí com mTLS (decisão 40) | **Cortado** | Os três simuladores já provam o port de provedor; a conta pode exigir PJ e a homologação tem limites estranhos |
| F10: deploy no Azure, HTTPS, backup, observabilidade, k6 (decisão 39) | **Cortado** | Custo, manutenção e superfície de ataque para um link que ninguém vai testar sob carga |
| CNAB 240, parser e gerador (decisão 29) | **Opcional**: só o retorno de cobrança (segmentos T e U), se sobrar tempo | Formato posicional grande e com dialeto por banco; a conciliação mostra o mesmo raciocínio contra o extrato do simulador |
| Página do pagador e painel em `html/template` (decisão 55) | **Cortado** | A demo por script cobre o mesmo roteiro |
| — | **F10: caos e conciliação** | Teste citado 5 (mesma seed, mesmo estado final); falhas injetadas nos três simuladores sem perder nem duplicar dinheiro; conciliação diária com os casos da seção 4.8 |
| — | **F11: entrega** | `docker compose up` com Postgres, API, worker e os três simuladores; script de demo da GameStore (PIX, liquidação, chargeback); README final curto; roteiro de perguntas da entrevista |

Isto já está no `docs/escopo.md` (decisão 58, com as decisões 29, 39, 40 e 55 marcadas
como revistas, e as seções 1, 5, 9, 10 e 12 atualizadas) e na tabela de status dos dois
READMEs. A página do pagador (decisão 55) saiu junto.

## 2. Linha de base (14/11/2026, master em `b8619b2`)

Contagem com `wc -l`, sem o código gerado pelo sqlc (`*db/`).

| O quê | Linhas | Depois (16/11/2026) |
|---|---|---|
| Go de produção | 17.147 | 16.427 |
| Comentários no Go de produção | 1.630 (~10%) | 924 (~6%) |
| Go de teste | 19.009 | 18.894 |
| Código gerado pelo sqlc | 4.796 | 4.796 |
| SQL: migrations / queries | 631 / 633 | 631 / 633 |
| `README.md` / `README.pt-BR.md` | 951 / 951 | 973 / 973 (95 KB → 90 KB) |
| `api/openapi.yaml` | 1.505 | 1.493 |
| Todos os arquivos versionados | 46.994 | 46.404 |

Maiores pacotes (produção / comentários / teste):

| Pacote | Produção | Comentários | Teste | Depois (16/11/2026) |
|---|---|---|---|---|
| `internal/payments/adapters` | 3.577 | 209 | 1.937 | 3.490 / 122 / 1.937 |
| `internal/payments/domain` | 2.009 | 300 | 1.831 | 1.859 / 159 / 1.831 |
| `internal/payments/app` | 1.396 | 238 | 82 | 1.300 / 141 / 82 |
| `internal/providers/cardsim/server` | 634 | 37 | 0 | 618 / 21 / 0 |
| `internal/providers/simkit` | 553 | 64 | 229 | 515 / 26 / 229 |
| `cmd/api` | 390 | 15 | 4.903 | 382 / 7 / 4.792 |

Para medir de novo, rode da raiz:

```sh
find . -name '*.go' ! -name '*_test.go' ! -path './*/*db/*' | xargs cat | wc -l
find . -name '*.go' ! -name '*_test.go' ! -path './*/*db/*' | xargs grep -h '^\s*//' | wc -l
find . -name '*_test.go' | xargs cat | wc -l
```

## 3. Regras

1. **Nenhuma mudança de comportamento.** O polimento só reescreve; corrigir um bug é uma PR
   separada, com teste que o mostre.
2. **Quanto menos código, melhor**, desde que fique claro. Apagar vence reescrever, e
   reaproveitar vence criar.
3. **Comentário só para o porquê:** um invariante, uma decisão (com o número), uma armadilha
   ou uma ordem de locks. Comentário que repete o nome ou o corpo da função sai. Doc
   comment em identificador exportado só quando acrescenta algo além do nome.
4. **Os testes provam a mesma coisa com menos linhas:** tabelas no lugar de testes quase
   iguais e helpers compartilhados no lugar de cópias. Nenhum teste citado no README perde
   força.
5. **PRs pequenas, uma por área**, cada uma com as linhas antes e depois na descrição.
6. Commits em português, no estilo `chore(scope): ...` ou `refactor(scope): ...`, **sem
   atribuição ao Claude**.

## 4. Ferramentas

- **Ponytail**, as regras de "código mínimo" para agentes. Instala dentro do Claude Code,
  não no terminal, com dois prompts separados:
  1. `/plugin marketplace add DietrichGebert/ponytail`
  2. `/plugin install ponytail@ponytail`

  Os hooks dele precisam do `node` no PATH. Antes de usar, ler as regras instaladas e
  conferir que não contradizem as regras da seção 3.
- **deadcode**: lista funções inalcançáveis. O modo automático bloqueia o Claude de baixar
  a ferramenta, então **o Claude deve pedir ao Felipe para rodar** no prompt, com `!` na
  frente, para a saída cair na conversa:
  `! go run golang.org/x/tools/cmd/deadcode@v0.52.0 -test ./...`

  A v0.28.0 não lê o Go 1.27 e quebra com `unexpected expr: *ast.KeyValueExpr`.
- **`dupl`** do golangci-lint, ligado só localmente para achar blocos repetidos:
  `golangci-lint run --enable-only dupl ./...`

## 5. Verificação de cada PR

```sh
golangci-lint fmt ./... && golangci-lint run ./...
sqlc diff
go test -race -count=1 ./...          # inclui o e2e
```

Nas PRs que mexem em `payments`, `ledger` ou `idempotency`, plante de propósito dois ou
três erros num caminho de dinheiro e confira que algum teste falha. Exemplos: tirar um
lock, inverter um sinal, pular uma verificação de idempotência. Depois desfaça.

## 6. Itens, em ordem

Marque `[x]` ao terminar e anote o número da PR.

- [x] **0. Escopo.** Decisão 58 no `docs/escopo.md` e status nos READMEs.
- [x] **1. Código morto.** (#58) O `deadcode` achou `TransactionID.String`, `ParseRefundID`
  e os `Sign` do `boletosim` e do `cardsim`; com o `ParseRefundID` saiu o
  `ErrInvalidRefundID`. O `ErrChargebackNotFound` foi para `app/chargeback.go`.
- [x] **2. Comentários.** (#59–#67) 1.630 → 924 linhas (−43%). O alvo inicial, menos de
  815, foi trocado por "só comentário de porquê": abaixo de 924 já seria preciso apagar
  ordens de lock, invariantes e decisões. Por pacote: `payments/domain` 300 → 159,
  `payments/app` 238 → 141, `payments/adapters` 209 → 122, `providers/cardsim` com o
  `server/` 108 → 43, `simkit` 64 → 26, `webhooks/app` 48 → 30, `boleto` 47 → 35,
  `pixsim`, `boletosim` e `cmd/*sim` 115 → 57, os demais pacotes 499 → 311. Saíram de
  passagem cinco comentários errados: o `Register` truncado, o `ParseMethod` que
  recusaria boleto e cartão, o CNAB como evidência da F9, o provedor real da F9 e a
  conciliação da F9, que agora é F10.
- [x] **3. Duplicação no código de produção.** Medido em 16/11/2026: não compensa.
  - `HandlePixSimEvent` e `HandleBoletoSimEvent`: o que muda está em quase toda linha
    (tipo, constante "paid", status, chave, `BRCode`/`Barcode`, mensagem). A versão
    unificada mais enxuta tem 35 linhas contra 34.
  - Adapters e servidores dos simuladores: cada um tem seus próprios tipos. Unificar o
    `Refund` no `simkit` muda o JSON (`charge_id`, `boleto_id`, `authorization_id`), o que
    a regra 1 não deixa.
  - Erro → problema nos `*_http.go`: já é uma linha por erro, com mensagem própria de
    cada endpoint. Só dois `case` se repetem (uns 5 linhas).
  - Struct de dependências nos construtores: 41 chamadas, quase todas em testes, passam a
    montar o struct; o total não diminui.
  - O `dupl` só acha esses handlers e `confirmationAccounts`/`refundAccounts`, que têm a
    mesma forma com contas diferentes.
- [x] **4. Testes.**
  - [x] (#68) `cmd/api` tem 4.903 linhas. Os helpers estão espalhados: `processInbox` e
    `boletoEvents` estão em `contract_boleto_test.go` mas roteiam os três provedores, e
    devem ir para `contract_test.go` com outro nome.
  - [x] (#68) Taxa fixa de 5,0000 repetida em `contractRates`, `testRates` (worker) e duas
    vezes em `contract_refund_resolver_test.go`: deixar uma só. Eram seis cópias no
    `cmd/api` e duas no `cmd/worker`; ficou uma por pacote.
  - [x] (#69) Testes quase iguais que viram tabela. Os testes de erro já eram tabelas.
    A repetição estava nos helpers que conferem o status e decodificam o JSON
    (`refundOf`, `chargebackOf`, `list`, `listSettlements`, `getBalance` e mais seis):
    viraram `decode[T]`. O `dupl` com limiar 60 só acha mais pares de tipos diferentes
    (`Status`/`AttemptStatus`, `hold`/`replay`), que não encurtam sem genéricos.
  - [x] (#68) Helpers de corpo e de requisição repetidos entre arquivos: `postChargeback`,
    `postRefund`, `getRequest` e semelhantes. Os oito viraram uma linha sobre
    `apiRequest`, e o `getPayment` saiu.
- [x] **5. OpenAPI.** (#70) Respostas `404`, `409` e `503` com o mesmo bloco inline repetido em
  cada caminho: mover para `components/responses` e referenciar. Os testes de contrato
  validam contra o arquivo, então qualquer erro aparece. Só dois blocos eram iguais de
  fato: a `404` de pagamento (3 vezes) e a `503` do adquirente (2 vezes). Viraram
  `PaymentNotFound` e `AcquirerUnavailable`. Os demais têm a descrição de cada endpoint,
  e no OpenAPI 3.0 um `$ref` não troca a descrição. Os padrões de `evt_` (3 vezes) e
  `ref_` (2 vezes) viraram `EventId` e `RefundId`. 1.505 → 1.493 linhas.
- [x] **6. Pendência de comportamento (PR à parte, não é polimento).** Confirmar se o job
  de expiração pega um cartão `authorized` por mais de 7 dias sem captura (decisão 52).
  Se não pegar, teste e correção. Pega: `TestUncapturedAuthorizationExpires` cobre, e
  falha quando o prazo do `Schedule` muda. Nada a corrigir.
- [x] **7. READMEs.** (#71) O começo deve se entender em 30 segundos: o que é, como rodar e os 5
  testes mais fortes. As seções de resultado ficam mais curtas e as decisões mais
  enxutas. Os números de linhas da seção 2 entram lá, antes e depois.
  Feito: "Quick start" de dois comandos e a tabela dos 5 testes citados no topo; nas
  seções de resultado saíram as listas de mutantes (fica o número e os bugs reais achados)
  e o "20/20 runs" repetido, que já é o padrão dito no começo (95 KB → 90 KB). As
  decisões ficaram: cada uma já é escolha + alternativa descartada em duas linhas, e
  cortar a alternativa tira o porquê. A F9 ficou `pronta` no Status, com os números.
- [x] **8. Medir de novo** e preencher o registro abaixo. (#71) Coluna "Depois" na seção 2.

## 7. Registro de progresso

| Data | Item | PR | Produção | Comentários | Teste | Observação |
|---|---|---|---|---|---|---|
| 14/11/2026 | Linha de base | — | 17.147 | 1.630 | 19.009 | master `b8619b2` |
| 15/11/2026 | 1. Código morto | #58 | 17.131 | 1.628 | 19.009 | `deadcode` só achou 4 funções |
| 15/11/2026 | 2. Comentários: `payments/domain` | #59 | 16.990 | 1.487 | 19.009 | 300 → 159 no pacote |
| 15/11/2026 | 2. Comentários: `payments/app` | #60 | 16.893 | 1.390 | 19.009 | 238 → 141 no pacote |
| 15/11/2026 | 2. Comentários: `payments/adapters` | #61 | 16.806 | 1.303 | 19.009 | 209 → 122 no pacote |
| 15/11/2026 | 2. Comentários: `providers/cardsim` | #62 | 16.741 | 1.238 | 19.009 | 108 → 43 no pacote |
| 15/11/2026 | 2. Comentários: `simkit` | #63 | 16.703 | 1.200 | 19.009 | 64 → 26 no pacote |
| 15/11/2026 | 2. Comentários: `webhooks/app` | #64 | 16.685 | 1.182 | 19.009 | 48 → 30 no pacote |
| 15/11/2026 | 2. Comentários: `boleto` | #65 | 16.673 | 1.170 | 19.009 | 47 → 35 no pacote |
| 15/11/2026 | 2. Comentários: providers | #66 | 16.615 | 1.112 | 19.009 | 115 → 57 no grupo |
| 15/11/2026 | 2. Comentários: demais pacotes | #67 | 16.427 | 924 | 19.009 | item 2 fechado |
| 16/11/2026 | 3. Duplicação | — | 16.427 | 924 | 19.009 | medido, não compensa |
| 16/11/2026 | 4. Testes: helpers do `cmd/api` | #68 | 16.427 | 924 | 18.956 | −53 no teste |
| 16/11/2026 | 4. Testes: `decode[T]` | #69 | 16.427 | 924 | 18.894 | item 4 fechado |
| 16/11/2026 | 5. OpenAPI e 6. expiração do cartão | #70 | 16.427 | 924 | 18.894 | spec −12; item 6 já coberto |
| 16/11/2026 | 7. READMEs e 8. medição final | #71 | 16.427 | 924 | 18.894 | F9 fechada: produção −4,2%, comentários −43%, teste −0,6% |

## 8. Como retomar numa sessão nova

Peça ao Claude:

> Lê `docs/polimento.md` e continua o polimento a partir do próximo item não marcado.
> Segue as regras da seção 3 e a verificação da seção 5, e atualiza o registro no fim.
