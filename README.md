# Cross-border Pay

[![CI](https://github.com/gengibrepower/cross-border-pay/actions/workflows/ci.yml/badge.svg)](https://github.com/gengibrepower/cross-border-pay/actions/workflows/ci.yml)
[![Nightly stress](https://github.com/gengibrepower/cross-border-pay/actions/workflows/nightly.yml/badge.svg)](https://github.com/gengibrepower/cross-border-pay/actions/workflows/nightly.yml)
![Go 1.27](https://img.shields.io/badge/go-1.27-00ADD8?logo=go&logoColor=white)
![PostgreSQL 18](https://img.shields.io/badge/postgres-18-4169E1?logo=postgresql&logoColor=white)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

🇧🇷 [Leia em português](README.pt-BR.md)

**A cross-border payment gateway in Go, built to stay correct under concurrency, duplicates and partial failures.**

A foreign merchant charges in US dollars. A payer in Brazil pays in reais by PIX, boleto or card. The system converts the currency, records every cent in a double-entry ledger and pays the merchant out in dollars.

The one rule that can never break: **the money on record always matches the money that exists**, even when providers time out, events arrive twice or out of order, and workers die halfway through.

> **Learning project, not a real gateway.** Money, customers and payment providers are all simulated. Fees and rates are illustrative. It exists to practice the engineering behind correct financial systems.

## Quick start

You need Docker.

```sh
docker compose up -d           # Postgres, the API, the worker and the three simulators
docker compose run --rm demo   # the GameStore story, told against all of them
```

The demo plays the merchant over the public API, and the payer and the acquirer at the simulators:

```
1. PIX
   A player buys 20.00 USD of credits. The gateway quotes it: the payer pays 101.50 BRL by PIX,
   the GameStore gets 19.40 USD after a fee of 0.60 USD.
   ...
   and the GameStore receives payment.paid, signed: the signature checks out.
   PIX settles D+1: the money is available on 2026-11-16 and paid out by that day's settlement.

2. Card and chargeback
   ...
   The issuer decides for the GameStore: chargeback.won, and the money comes back.
```

To work on the code you also need [mise](https://mise.jdx.dev): `mise install` brings Go and the tools at the versions CI uses, and `mise run test` runs the whole suite with `-race` against a real Postgres. To run each part by hand, see [Run it locally](#run-it-locally).

## What the project sets out to prove

1. **Idempotency across the whole chain.** API, provider calls, incoming events, outgoing webhooks and jobs. Retrying never charges, posts or pays out twice.
2. **Ledger consistency under concurrency.** Thousands of simultaneous operations, duplicates included, and the ledger still sums to zero per currency.
3. **Dual writes without distributed transactions.** The database and the provider never share a transaction. Intent is recorded first, ambiguous results become `unknown` instead of failures, and inbox/outbox tables carry events in and out.
4. **Tolerance to a hostile outside world.** Providers that lag, duplicate, reorder or vanish, and merchants whose endpoints are down.

## What is proven

Every number below is measured, not estimated. Every test that touches the database runs against a **real PostgreSQL 18** in a container (`testcontainers-go`), never a mocked one; domain tests do no I/O at all.

The five tests the project is measured by ([docs/escopo.md §9](docs/escopo.md)):

| # | Test | Measured |
|---|---|---|
| 1 | `TestThousandConcurrentConfirmationsPostOnce`: a payment is confirmed once | 1 ledger posting out of 1,000 concurrent confirmations |
| 2 | `TestConcurrentPartialRefundsNeverExceedThePayment`: refunds never pass what was paid | 4,000 concurrent refund requests |
| 3 | `TestThousandsOfConcurrentTransfers`: the ledger closes at zero | 100,000 concurrent transfers |
| 4 | `TestWorkerKilledMidRoundDeliversEveryEventAfterRestart`: a killed worker loses no webhook | 100 events, killed mid-round: all delivered |
| 5 | `TestChaosSameSeedSameStory`: same seed, same history and final state | 24 operations under faults the seed draws, twice on fresh stacks: the same state; another seed, another state |

Beyond them, a retry never charges twice (10 concurrent requests with one key: 1 payment) and a settlement pays once (the same cutoff twice, or on 8 workers: 1 payout).

Each group below opens to what every test shows and what it measured. Runs are with `-race -count=20` unless stated, and "mutants killed" are deliberate bugs each test was checked to catch.

<details>
<summary><b>Ledger and money</b> · 5 tests</summary>

**`TestThousandsOfConcurrentTransfers`**

10 concurrent workers post 5,000 random transfers between 20 accounts in two currencies. Afterwards the database itself is asked whether every invariant holds.

**Result:** 20/20 runs with `-race -count=20`, i.e. **100,000 concurrent transfers**. The ledger closes at zero per currency every time, and transfers that would overdraw are rejected, never half-applied.

---

**`TestLedgerInvariantsAgainstPostgres`**

Property-based: random sequences of valid and invalid transactions against Postgres. After every step, balances match an in-memory model.

**Result:** 100 random sequences per run, 20/20 runs.

---

**Ledger domain properties**

Random transactions keep the invariants in memory, and an unbalanced transaction is always rejected.

**Result:** 2 properties × 100 cases per run, 20/20 runs.

---

**FX properties**

Conversion is nearest half-even, odd-symmetric and monotonic, and round-trips when the rate is at least 1. A quote's USD legs sum to zero, and with a base rate of at least 1 the rounding account never takes more than one cent. A stored quote reads back exactly as it was computed, without repricing.

**Result:** 7 properties × 100 cases per run, 20/20 runs.

---

**`TestEntriesAreImmutable`**

Postgres triggers reject `UPDATE`, `DELETE` and `TRUNCATE` on entries and transactions. Corrections are new transactions.

**Result:** Enforced in the schema itself.

</details>

<details>
<summary><b>API, authentication and idempotency</b> · 6 tests</summary>

**`TestTransitionIsExhaustive`**

Every (status, event) pair of the payment state machine is checked against section 3.4, written out again inside the test: anything outside the table is `ErrInvalidTransition`, and a repeated event the status already reflects is a no-op.

**Result:** All 49 pairs, 20/20 runs.

---

**`TestAuthenticate`**

API keys against real Postgres: a malformed key, an unknown one, the right lookup with a wrong secret, the right key with its mode changed, and a revoked key all get the same `401`, so the response does not tell an attacker which part was wrong.

**Result:** 5 rejection cases, plus a mutation check: a hash comparison that accepts any secret makes it fail.

---

**Contract tests (`cmd/api`)**

Every request and response of `POST /v1/payments`, `GET /v1/payments/{id}`, `GET /v1/payments`, `POST /v1/payments/{id}/cancel` and `POST /v1/payments/{id}/refunds` goes through the real handler and a real Postgres and is checked against the hand-written `api/openapi.yaml` with `kin-openapi`. The GameStore payment returns R$ 101.50 with a US$ 0.60 fee and a 30-minute quote, and the `GET` returns byte for byte what the `POST` returned. Walking 7 payments in pages of 3 sees each one exactly once, newest first, and none of another merchant's; another merchant's payment, an unknown ID and a malformed ID all get the same `404`.

**Result:** `POST`: 1 success and 14 error cases, and no rejected request stores a payment. `GET`: 4 `404`/`401` cases and 6 invalid-parameter cases, each with its stable `code`. Changing `id <` to `id <=` in the cursor makes the pagination test fail.

---

**Idempotency at the store layer (`internal/idempotency`)**

The eight cases of section 4.2 against a real Postgres: a stored response replayed as is, a different body rejected, a 4xx kept, a 5xx before any work releases the key, a 5xx after recorded work keeps it, keys of different merchants independent. A key left by a request that died is resumed from where it stopped once the lease ends, and the dead request can no longer complete or release it. The recovery point rolls back with the transaction of whoever wrote it.

**Result:** 20 concurrent requests with one key: exactly 1 proceeds and 19 get `409`. Dropping the `lock_token` from completion or the `recovery_point` from release makes the test for that case fail.

---

**Idempotency through the API (`POST /v1/payments`)**

A retry gets the same 201 byte for byte, marked `Idempotent-Replayed: true`, and no new payment; the same key with another payment is a `422`; a 4xx is stored and replayed; different merchants with the same key create different payments. **Simulated crash:** the request creates the payment and dies before storing its answer; the retry gets `409` while the lease runs and, after it, `201` with that same payment, identical to the `GET`, and the database still holds 1 payment and 1 quote. In the middleware, a 5xx before any work releases the key, and a 5xx after a recovery point keeps it and lets the retry resume at once.

**Result:** 10 concurrent HTTP requests with one key create exactly 1 payment. Making `Create` ignore the recovery point makes the crash test fail.

---

**Key cleanup (`cmd/worker`)**

Every hour, in batches of 1,000, the job deletes keys older than 24 h and nothing younger. With `FOR UPDATE SKIP LOCKED`, a key another transaction has locked is passed over without waiting and deleted on the next round. A failed round is logged and does not stop the worker.

**Result:** 7 expired keys deleted in batches of 3. Dropping `SKIP LOCKED` makes `Cleanup` wait for the lock until its deadline, and the test fails.

</details>

<details>
<summary><b>PIX and the provider</b> · 7 tests</summary>

**BR Code (`internal/brcode`)**

The 5 complete examples of the Banco Central's [Manual de Padrões para Iniciação do Pix v2.10.0](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaodoPix.pdf) parse with their CRC checked, and the encoder reproduces the static and dynamic ones character for character. The CRC16 covers the `6304`, as the manual says; a CRC computed without it is rejected. Field 54 without floats (`10150` ↔ `"101.50"`).

**Result:** Property test `Parse(Encode(c)) == c`: 100 cases per run, 20/20 runs; 14 invalid payloads and 14 invalid codes rejected, each for the right reason.

---

**Provider attempts (`payments`)**

Every (status, event) pair of the attempt state machine is checked against section 3.5, written out again inside the test; an ambiguous answer never rejects an attempt, and the payment event may arrive before the create response. Against a real Postgres, a payment has at most one live attempt (a partial unique index: only a rejection frees it), and events on one attempt are serialized by a row lock.

**Result:** All 30 pairs. 20 concurrent creates for one payment leave exactly 1 attempt, and 8 concurrent accept/reject events on one attempt change it exactly once, 25 attempts per run. Dropping the `FOR UPDATE` or the index's `WHERE` makes these tests fail.

---

**PIX simulator (`cmd/pixsim`)**

A real HTTP provider with charges in memory and faults on command: `reject`, `fail_before` (500, nothing done), `timeout_after` (the charge is created and the answer never comes) and `delay`. After a `timeout_after`, looking the charge up by our attempt ID finds it, and retrying with the same key returns that same charge. Every BR Code it issues parses with `internal/brcode`. Only an explicit refusal reaches the payments port as `Rejected`; a 500, a timeout or an unknown answer is an error, so the attempt goes to `unknown`.

**Result:** Same seed and same 50 keys sent concurrently in reverse order give the same charges, byte for byte, as sent one by one; 8 malformed requests refused with `400`. 10 mutants killed.

---

**Simulator refunds (`cmd/pixsim`)**

A paid charge can be refunded in parts, as a PIX devolução, once per our refund ID: the same key returns the same refund, another amount under it is `idempotency_key_reused`. The simulator holds invariant 8 on its own side: it refuses a refund of an unpaid charge, in another currency or above what is left, and stores the refusal with its reason, so a lookup by our ID finds the same decision. The same faults apply (`refund`, `get_refund`): after a `timeout_after` the money went back and the lookup finds it `completed`. The payments port gains `Refund` and `GetRefund`, with the same rule as attempts: only a refusal is a result, anything else is ambiguous.

**Result:** 50 concurrent refunds of R$ 10.00 on a R$ 101.50 charge: exactly 10 completed. 7 mutants killed.

---

**Simulator events**

Paying a charge posts one `charge.paid` event to `PIXSIM_WEBHOOK_URL`, signed with HMAC-SHA256 over the timestamp and the body. `pixsim.VerifyEvent` refuses a tampered body, a wrong secret, a changed timestamp and any signature more than 5 minutes away from now, so a captured delivery cannot be replayed later. The event ID comes from the seed and a redelivery (`/control/events/{id}/deliver`) sends the same bytes signed again, so the receiver can drop duplicates by ID. Paying twice sends nothing new; a webhook that fails does not undo the payment; a payer that gives up does not cut the delivery. The gateway receives them at `POST /v1/providers/pixsim/events` (the inbox, below).

**Result:** 8 forgeries refused and 5 malformed signed bodies refused, among them a decimal amount. 17 mutants killed.

---

**PIX attempt on create (`POST /v1/payments`)**

Each payment gets its provider attempt and a BR Code that parses for R$ 101.50, identical in the `POST`, the `GET` and the list. A refusal is a `201` with `pix.status: failed`; a timeout after the provider acted, or a `500` from it, is a `201` with `processing` and the attempt in `unknown`, bounded by the 300 ms test timeout. **Crashes at each step:** after the first transaction (attempt `created`) the retry calls the provider once; after `sent` it does not call again and marks `unknown`; after the answer is stored it only replies. A client that disconnects mid-call does not stop it: the attempt still ends `accepted`.

**Result:** 10 concurrent requests with one key and a retry: 1 provider call, 1 attempt. 11 mutants killed.

---

**Unknown resolver (`cmd/worker`)**

Ambiguous attempts are looked up at the provider, never created again (decision 17). A charge the provider made while our call timed out ends `accepted` with its BR Code, or `succeeded` if paid meanwhile. Not found is retried during a 1-minute grace and then means `rejected`. A request that died after writing `sent` is picked up once it is a minute stale. Failed lookups wait 5 s, 10 s, 20 s… up to 5 min, and at 24 h the attempt is escalated and never looked up again. A worker that dies holding a claim only delays the attempt until its lease ends.

**Result:** Backoff: 292 lookups over 24 h, every wait checked. 4 concurrent resolvers over 50 attempts: 50 claims and 50 lookups, one per attempt. 10 mutants killed.

</details>

<details>
<summary><b>Inbox, confirmation and outbox</b> · 3 tests</summary>

**Inbox (`internal/events`)**

The webhook checks the signature, stores the event with the exact bytes and header it came with, and answers `200`, for a duplicate too; only a failure to store asks for a redelivery. A worker claims due events with `FOR UPDATE SKIP LOCKED` and applies each one in the transaction that records its status, so the effect and the status commit together or not at all. `charge.paid` succeeds the attempt it names once the amount matches the quote. One for an attempt not stored yet is retried after 5 s, 10 s, 20 s… up to 5 min, and fails at 24 h; one that contradicts the attempt (another amount or charge ID, a rejected attempt) fails at once and changes nothing. Events of one payment are applied one at a time behind its row lock, and that lock does not wait for a foreign key check.

**Result:** 100 concurrent deliveries of one event: all `200`, 1 row. 5 refused requests (signature, unreadable, no ID, unknown provider, over 64 KiB), none stored. 4 processors over 50 events: each applied once. 6 divergent events leave the attempt as it was. 25 mutants killed.

---

**`TestThousandConcurrentConfirmationsPostOnce`**

Confirmation (`payments` + `ledger`): an attempt that becomes `succeeded`, by the provider's event or by the resolver, moves the payment to `paid` and posts section 3.3's transaction in that same database transaction. 1,000 confirmations of one charge run at once, half through each path. A ledger that cannot post leaves the attempt and the payment as they were; a payment already `paid` is not posted again; each merchant is owed on its own account, created by its first confirmation. Every path locks the payment, then the attempt, then the accounts: a test holds the payment's lock and checks with `NOWAIT` that a waiting confirmation has not taken the attempt's.

**Result:** **Exactly 1 ledger transaction and 1 `payment.paid` out of 1,000 confirmations**, with the GameStore balances to the cent (R$ 101.50 in, US$ 19.40 owed, US$ 0.60 fee, US$ 0.30 spread). Any quote confirms into a balanced transaction with no zero leg: 100 cases per run, 20/20 runs. 50 concurrent first confirmations of one merchant create 1 account. 14 mutants killed.

---

**Outbox (`payment.paid`)**

The confirmation appends `payment.paid` to `outbox_events` in its own transaction (section 4.5), with `payment_version` bumped by the same `UPDATE` that changes the status. The payload is stored as the exact bytes every delivery will send and sign, and validated against the `PaymentEvent` schema in `api/openapi.yaml`; its `payment_id`, `status` and `amount` match the `GET` of the payment. An outbox that refuses the event undoes the attempt, the payment and the ledger posting. Delivery is in the "Webhook delivery" row.

**Result:** 1 event per payment under the 1,000 concurrent confirmations above. 10 mutants killed.

</details>

<details>
<summary><b>Expiry, cancel and late money</b> · 3 tests</summary>

**Expiry (`cmd/worker`)**

A job claims pending payments whose quote has expired (`FOR NO KEY UPDATE SKIP LOCKED`, then a 1-minute lease) and asks the provider before deciding: paid on time means confirmed, otherwise expired with `payment.expired` in the outbox. On time is the provider's `paid_at` against the quote's `expires_at`, never when the news reached us, and `confirm` applies the same rule to events, so the job and the event agree whichever comes first. A provider that does not answer decides nothing: the payment waits for the lease. **The race:** the payer pays 2 s before expiry and the event has not arrived when the job runs; the job finds the payment and confirms it, and the event that comes 3 s later is a no-op. Paid 1 s late, by job or by event, it is never confirmed: the money is held for review (next row).

**Result:** 4 concurrent jobs over 50 payments: each expired once, with one lookup each. 17 mutants killed.

---

**Cancel (`POST /v1/payments/{id}/cancel`)**

A pending payment becomes `canceled` with `payment.canceled` in the outbox. Like the expiry job, the cancel asks the provider first: a payer who already paid while the news was on its way is confirmed and the cancel answers `409 payment_not_cancelable`. A provider that does not answer gets `503 provider_unavailable` and nothing changes. Canceling twice answers `200` with no second event, so no `Idempotency-Key` is needed. Money sent after the cancel is late money: held for review, the payment stays canceled. **The race:** the cancel and an on-time confirmation take the same payment lock, so exactly one wins.

**Result:** `TestCancelAndConfirmationRace`: 400 rounds of cancel × confirmation: 356 canceled with the money held, 44 paid with one posting, never both. 3 mutants killed.

---

**Money without a destination (section 4.4)**

Money the provider confirmed always enters the ledger; what is in doubt is only where it goes. A payment that cannot confirm its payment (late, another amount, for a rejected attempt, or a second charge) is posted from `provider_receivables:BRL` to `divergence:BRL` and opens a review case in `review_cases` (the `reconciliation` module, in the same transaction). The payment is untouched and the attempt `succeeded` when the state machine allows it. One case per provider charge: its unique key is the first barrier and the ledger's unique reference the second, so the event, the resolver and the expiry job post once together. The amount check now runs in the domain for all three paths; before, only the event compared amounts, and the resolver confirmed any payment the provider reported as paid. An event that cannot be held (another currency) is `failed` and writes nothing: a savepoint undoes what it wrote before, since the inbox commits the failed status.

**Result:** Paid 1 s late, by job first, by event first and both at once: 1 case and R$ 101.50 held each time, ledger summing to zero per currency. 20 concurrent deliveries of one divergent event: 1 case, 1 posting. Through the worker with the simulator's own event: payment `expired`, attempt `succeeded`, R$ 101.50 on `divergence:BRL`. 16 mutants killed.

</details>

<details>
<summary><b>Refunds and settlement</b> · 6 tests</summary>

**Refund pricing and postings (`payments`, decision 13)**

A refund is priced by the proportion of the original payment: US$ 10.00 of the GameStore returns R$ 50.75 to the payer and US$ 0.15 of the spread. It posts in three steps: the hold takes the refunded dollars from the merchant into "refunds in flight" when the refund is created; the completion sends the reais back from the provider, buys them back at today's base rate and puts the difference into the exchange result (at 5.5000 the gateway gains US$ 0.92, at 4.5000 it loses US$ 1.13); a refusal releases the hold. **The rounding bug it avoids:** rounding each refund on its own, two halves of R$ 101.51 are R$ 50.76 each, R$ 101.52 in all; each share is cumulative instead, what all refunds so far should have returned minus what the earlier ones did, so they add up to the exact total. The refund state machine is every (status, event) pair checked against a table written out again in the test.

**Result:** `TestRefundsOfAnyPaymentAddUp`: 10,000 random payments (rate, spread, fee, amount) with up to 12 refunds each, some refused afterwards: what counts never passes what was paid, every posting balances per currency, and refunding the rest returns exactly the reais and the spread. 14 mutants killed.

---

**`TestConcurrentPartialRefundsNeverExceedThePayment` (`POST /v1/payments/{id}/refunds`)**

Invariant 8 through the whole API: the refund locks the payment (`FOR NO KEY UPDATE`), sums the refunds that still count (all but the refused), prices the new one and posts its hold in one transaction; the provider is called after the commit, and its answer posts the money gone back or releases the hold, moving the payment to `partially_refunded` or `refunded` with `refund.succeeded` or `refund.failed` in the outbox. 40 refunds of random amounts hit one paid payment at once, over HTTP with their own `Idempotency-Key`s. A refusal by the provider is a `201` with `failed`, the hold back with the merchant; a timeout leaves the refund `pending`, the hold kept; a retry with the same key replays the answer; a request that died after its first transaction resumes without calling the provider twice, and after `sent` does not call it at all.

**Result:** **4,000 concurrent refund requests** (5 payments × 40 per run): never more than US$ 20.00 or R$ 101.50 refunded per payment, every refund the gateway accepted completed at the provider, and every refusal still would not fit in what was left. Afterwards the books close to the cent: nothing in flight, the merchant owes back exactly what was refunded, the provider holds what was not returned. 11 mutants killed.

---

**Refund resolver (`cmd/worker`, decision 17)**

A refund left without an answer does not wait for the merchant to retry: the same queue as attempts (`next_check_at` as schedule and lease, `SKIP LOCKED`) finishes what the request left. A refund `created` for 1 min, from a request that died before calling the provider, is sent, and only by whoever moves it to `sent`, so a worker and a retried request never both call the provider. A stale `sent` becomes `unknown` and is never sent again; `unknown` is looked up with the attempts' backoff. "Not found" rejects the refund only 1 min after its last change, not its creation: a refund sent late may be arriving right now. Rejected, the hold goes back to the merchant with `refund.failed`; completed, it posts as the request would, with `refund.succeeded`. With no answer in 24 h it goes to a person, the hold kept.

**Result:** 8 tests against the simulator and Postgres. 10 mutants killed.

---

**Daily settlement (`cmd/worker`, section 4.7, decisions 20 and 25)**

Once per merchant and cutoff date in `America/Sao_Paulo`, what is available becomes a payout in USD: it leaves `merchant_payable` for `settlements_paid`, and the payments that became available point at the settlement, each in exactly one. A PIX paid on local day P is available on P+1 (boleto P+2, card P+30, calendar days). Available is the payable's balance less what is still pending, each pending payment at its net less the refunds that count; so a refund of a pending payment comes out of it, and one of a payment already settled comes out of the next payout. A negative available pays nothing and waits for more money. The transaction locks the payments first and the payable after, the order of every path, and with the payable locked nothing else posts to it; `UNIQUE (merchant_id, cutoff_date)` makes a second run pay nothing.

**Result:** **F7's criterion**: running the same cutoff twice, or on 8 workers at once, makes one payout. A model of 4 rounds × 6 days with random payments and refunds before and after each cutoff checks every payout to the cent and with the right payments; 4 workers settling while payments are confirmed and refunded end with everything paid once and the payable at zero. 11 mutants killed.

---

**Balance and settlements (`GET /v1/balance`, `GET /v1/settlements`)**

The balance splits the merchant's payable as the settlement job does at that moment: pending is the net of the payments not available yet less their refunds, available is the rest, negative after a refund or chargeback of settled money that exceeds what came in since; reserved is the rolling reserve below. Both sums come from one `REPEATABLE READ` snapshot without the payable's lock, so a read never waits on a confirmation or holds up a settlement. The list shows the settlements newest cutoff first, each with its payments, read in the same snapshot as the page; the cursor is the cutoff date, unique per merchant, on the `UNIQUE (merchant_id, cutoff_date)` index; the payments come from a new partial index on `payments.settlement_id` (migration 00020)

**Result:** A balance checked at 9 points across 3 São Paulo days, among them 23:30 local, when the date in UTC has already turned, always adding up to the payable. 3 settlements in pages of 1: each seen once, newest first, with the same amount and payments as stored, none of another merchant's settled in the same runs. 5 invalid parameters and 2 `401`s. 11 mutants killed.

---

**Rolling reserve and debt limit (decisions 26 and 27)**

Each payout holds 10% of what is available on `merchant_reserve` for 90 days, and a later run gives it back, oldest first; the settlement stores what it held and until when (migration 00024). An available balance that a refund or chargeback made negative takes the reserve first, then waits for the next payments. A merchant whose payable plus reserve is below −US$ 1,000.00 takes no new payment (`422 debt_limit_exceeded`). The settlement locks the payments, then the payable and the reserve in account order, as every path.

**Result:** 3 new contract tests and 6 domain tests, and the reserve added to the settlement tests above: a negative available covered by the reserve and the rest waiting; reserves released after 90 days, the oldest first and none early; the reserve in the 4-round model and in the concurrent run, where every payout plus the reserve adds up to what came in; and a merchant past the limit refused for PIX and card, the refusal stored for the retry, nothing stored as a payment and another merchant unaffected, still refused after the reserve covered what it could. The release checked by property: what is released and held always adds up to what is left.

</details>

<details>
<summary><b>Webhooks</b> · 4 tests</summary>

**Webhook endpoint and SSRF at registration**

`PUT /v1/webhook-endpoint` stores the merchant's URL, refused when it is not `https` or points inside a network: loopback, private ranges, link-local and cloud metadata (`169.254.169.254`, also as `::ffff:169.254.169.254`), CGNAT, reserved ranges, `localhost`/`.internal`/`.local` names and numeric hosts such as `2130706433`, which some resolvers read as `127.0.0.1`. No DNS at registration: a name can resolve elsewhere later, so delivery checks the IP after DNS (row below). The signing secret is not stored: it is derived from a key outside the database, the merchant and a version; rotating is conditional on the current version, so a retried rotation shows the same secret instead of rotating twice.

**Result:** 35 IPs at the edges of each range and 31 URLs, each under the strict and the development policy. 20 concurrent rotations from one version: 1 rotation, the same secret in all 20 answers. A URL change that read the version before a concurrent rotation does not undo it, forced by holding the change open until the rotation waits on the lock. No `whsec_` in the stored row or the logs. 22 mutants killed.

---

**Webhook delivery (`internal/webhooks`)**

The worker claims due outbox events with `FOR UPDATE SKIP LOCKED` and a lease (`next_attempt_at` pushed past the timeout + 1 min), commits and only then posts, outside any transaction (section 4.3); the result goes into another short transaction. `attempts` fences the claim: once the lease ran out and another worker took the event, the first can no longer reschedule or fail it, while a 2xx, being a fact, is still recorded. Every post carries `Webhook-Signature: t=…,v1=…` (HMAC-SHA256 of the timestamp and the stored body), with a second `v1` from the previous secret for 24 h after a rotation. Only a 2xx is success: redirects are not followed, and the IP is checked again at `connect`, after DNS, under the same policy as registration. A failure waits a draw from [0, 30 s · 2ⁿ⁻¹], up to 6 h (decision 23), and becomes `failed` 72 h after creation. Each round takes up to 4 events per merchant, merchants with the oldest event first.

**Result:** **Cited test 4:** the worker is killed mid-way through the second round over 100 events of 5 merchants, with 10 posts received by the merchant and never answered; after the restart and the lease running out, all 100 are delivered, the 10 cut off twice and the other 90 once, and the cut-off ones get no retry or error recorded. 4 workers over 300 events of 10 merchants: each posted once, no bad signature. 13 decision cases, 7 forgeries refused, 10 addresses through the post-DNS check. 21 mutants killed.

---

**Disabled destination and cleanup (decision 24)**

Each round tells each merchant's breaker once: a 2xx ends the run of failures, and a failure 72 h after the first one with no success between disables the endpoint, in one conditional `UPDATE`, so only one of many concurrent failures disables and warns. Disabled, the endpoint gets no post: its events wait with the usual backoff and fail at 72 h, and failures cut short by shutdown do not count. The notice is `GET /v1/webhook-endpoint`, which shows `failing_since`, `disabled_at` and `disabled_reason`, plus an `ERROR` log line for the operator; any `PUT`, with the same URL too, turns it back on. An hourly job deletes, in batches of 1,000 with `SKIP LOCKED`, events delivered more than 30 days ago; `pending` and `failed` stay. The `GET` test found a bug from #32: times read from the database came out in the local zone (`-03:00`); they now come out in UTC (section 4.9).

**Result:** 20 concurrent failures at the 72 h mark: 1 disables. No post to a disabled destination, and delivery resumes after the `PUT`. A round with 2xx and 500 mixed ends the run. 5 delivered at the cutoff deleted over 3 batches; the one delivered 1 µs after it, the `failed` and the `pending` stay. 15 mutants killed.

---

**Events and manual redelivery (`GET /v1/events`, `POST /v1/events/{id}/redeliver`)**

The list shows the merchant's events, newest first, with an opaque cursor and a `delivery_status` filter: each with the exact body every delivery sends and where its delivery stands (attempts, next attempt, last status and error). Redelivery works from any status: `failed` is retried, `delivered` sent again, `pending` sent without waiting for its backoff; it answers `202`, with no `Idempotency-Key`, since repeating it only queues the event again. It opens a new retry window: the backoff restarts at 30 s and the 72 h count from it, without resetting `attempts`, which keeps fencing claims. A post in flight before the redelivery cannot push the "now" back to its own backoff. With no endpoint or a disabled one, `409`; another merchant's event, an unknown one or a malformed ID, the same `404`. `evt_`/`pay_`/`att_` now come from one place, `internal/platform/publicid`.

**Result:** 7 events in pages of 3: each seen once, newest first, none of another merchant's, each `event` equal to the stored bytes. 6 invalid parameters refused. Redelivery from all 3 statuses. An event that failed its whole window (over 20 attempts), redelivered, waits 30 s after its first failure, stays `pending` until 72 h after the redelivery and is delivered. 15 mutants killed.

</details>

<details>
<summary><b>Card</b> · 3 tests</summary>

**Card simulator (`cmd/cardsim`)**

An acquirer over real HTTP, with authorizations in memory and the faults of `internal/providers/simkit`, shared with the PIX simulator. Only a fictitious token reaches it, never a card number: `tok_approved` is captured, `tok_declined` and `tok_insufficient_funds` are declined, any other token is declined as invalid. An authorization is captured once, up to what was authorized: the same capture again gets the same answer, another amount is refused, and 7 days on (decision 52) it has expired, whoever reads it first. Refunds hold invariant 8 on the acquirer's side, and a refusal is stored for the lookup.

**Result:** 50 concurrent refunds of R$ 10.00 on R$ 101.50: exactly 10 completed. The same seed and 50 keys sent concurrently in reverse order give the same authorizations as sent one by one. A `timeout_after` authorizes and the lookup by key finds it; a `500` is neither a decline nor a refusal. A void releases only a live authorization, once, and a voided one cannot be captured. 5/5 runs with `-race`.

---

**Card payment on create (`POST /v1/payments`, decision 18)**

`method: card` with `card.token` is authorized and captured as the payment is created, so the `201` is already `paid` with `card.status: captured`, the GameStore's books posted and `payment.paid` in the outbox, byte for byte the same as the `GET` and as a retry. The payments module picks the provider by method (`pix` → `pixsim`, `card` → `cardsim`) and, once an attempt exists, by the name stored with it, so a refund goes back through the acquirer that captured. The token is stored with the attempt, refused by a `CHECK` unless it is a token, so a request that died before calling the acquirer is completed by its retry. A decline is a `201` pending with `declined`; a lost answer is `processing`, and the resolver finds the capture by the attempt's key.

**Result:** 6 tests against a real Postgres and both simulators: captured on create, 3 declines, 5 refused requests that store nothing (a card number among them), the resolver confirming a lost capture with 1 authorization, a resumed create authorizing once with the stored token, and a card refund posting as a PIX one. 6 mutants killed.


---

**Separate and partial capture (`POST /v1/payments/{id}/capture`, decisions 18 and 52)**

`card.capture_method: manual` only authorizes: the payment is pending with `card.status: authorized`, its quote locked for the authorization's 7 days, and nothing is posted while the card is only held. The capture takes the whole payment, or a part of it: the part is priced again at the same locked rate and pricing, so the payer pays its BRL, the fee is charged on what was captured, never on what was only held, and the rest of the hold is released. That quote of what was captured is stored before the acquirer is called (`payments.captured_quote_id`), and everything that posts the payment's money reads it: the confirmation, the refunds, which stop at what was captured (invariant 8), and the balance. An authorization is captured once, so the endpoint needs no Idempotency-Key: the same capture again answers as it is, another amount is a `409`. A lost answer leaves the attempt `unknown` for the resolver, and a capture the acquirer never received can only be retried for the same amount, because the first could still arrive. Canceling an authorized payment voids it at once; one nobody captured expires with its quote.

**Result:** 7 tests against a real Postgres and both simulators: a whole capture posting exactly the GameStore's R$ 101.50 and US$ 19.40, retried with no effect; US$ 15.00 of US$ 20.00 captured as R$ 76.12 with a US$ 0.45 fee, US$ 14.55 pending in the balance, a refund of US$ 15.01 refused and one of US$ 15.00 leaving the payment `refunded`; a lost capture confirmed by the resolver for the amount it stored; an uncertain capture refused for another amount and finished for the same one; a cancel that voids at the acquirer; an expiry after 7 days with nothing posted; and 8 refused captures. 14 mutants: 13 killed (the full refund compared with the amount asked for was a real bug, caught before it shipped), 1 equivalent.

</details>

<details>
<summary><b>Boleto</b> · 3 tests</summary>

**Barcode, linha digitável and calendar (`internal/boleto`, `internal/calendar`, decision 21)**

The 44-digit barcode and 47-digit linha digitável of Febraban's layout, with each field's modulo 10 check digit and the general modulo 11, and the due date factor restarted at 1000 on 22 February 2025: one count from 3 July 2000, repeating every 9,000 days, read as the date nearest today. The calendar is a table versioned in the code (`BR-2024`, with 20 November national under Lei 14.759/2023), with Carnival, Good Friday and Corpus Christi computed from Easter and 31 December without banking; a `Date` has no time or zone, and the day that counts is `America/Sao_Paulo`'s. The payer's CPF is checked by its two digits and never printed whole: `String` and `slog` show `***.982.247-**`.

**Result:** Banco do Brasil's published example (R$ 1.00, factor 3737) matches digit for digit both ways; every one-digit change of the linha digitável is caught; 100 random boletos a round go through the line and back; business days checked by property over 100 years and Easter against 11 published years from 1961 to 2285. The CPF property test found that the Receita's modulo 11 lets a few one-digit errors in the base through, since remainders 0 and 1 both give 0, so the test now claims only what the algorithm guarantees. 5 mutants killed.

---

**Boleto simulator (`cmd/boletosim`)**

A bank over real HTTP with the other two's `simkit`: it registers a boleto once per key, with its payer and due date, and returns the barcode and linha digitável it builds (bank `999`, which does not exist, and a nosso número drawn from the seed). It receives a boleto until the end of the first banking day on or after the due date and refuses it after (decision 53): one due on a Saturday is paid on Monday and refused on Tuesday, and a lapsed one reads `expired` to whoever asks first. Paying sends `boleto.paid` signed in `Boletosim-Signature`, with no payer in the body; refunds go back by transfer, never above what was paid.

**Result:** 8 client tests against the server and 3 for the command: the returned code read back by `internal/boleto` with the right amount, due date and free field; the same seed gives the same boletos and another seed others; the Saturday, the Tuesday and the 20 November holiday; 7 invalid requests refused; a `fail_before` that leaves no boleto and a stored `reject`; refunds stopping at what was paid; and one event per payment. 20/20 runs with `-race`. 1 mutant killed: refusing on the due date itself instead of the next banking day.

---

**Boleto payment (`POST /v1/payments`, decisions 44, 53 and 54)**

`method: boleto` with `payer` (name, CPF and an optional e-mail) comes back `201` pending with `boleto.status: ready`, the barcode, the printed linha digitável and a `due_date` three calendar days later in São Paulo; the quote lasts until the end of the bank's last day to receive it. The payer and the due date are stored with the attempt (migration 00023, with `CHECK`s that keep them together and apart from the card), so a retry registers the same boleto, and so is the returned barcode, so the `GET` shows the boleto the payer may already have printed; the linha digitável is derived from it. The CPF is in no response. `boleto.paid` reaches the inbox at `POST /v1/providers/boletosim/events` and the worker confirms the payment as any other, but only on an attempt `boletosim` holds: an event naming another provider's attempt fails. A lapsed boleto is refused by the bank and the payment expires through the expiry job; a lost answer stays `processing` until the resolver finds the boleto by its key.

**Result:** 5 contract tests against a real Postgres and the simulator, plus 1 worker test and 2 domain ones: the boleto ready, the same on `GET`, with the payer stored and the CPF absent from the response; paid at the bank, confirmed by the event with the GameStore's books and half refunded back through the bank; refused after its due date and expired with nothing posted; a lost answer resolved with 1 registration at the bank; 7 refused requests that never call the bank; and a `boleto.paid` naming a PIX attempt left `failed`. Due dates on a Saturday, on a holiday and at 23:30 on a Friday in São Paulo. 20/20 runs with `-race`. 8 mutants killed. One was a real flaw, found while writing the test: an event applied to another provider's attempt, which never showed with a single provider.

</details>

<details>
<summary><b>Chargebacks</b> · 2 tests</summary>

**Disputes at the card simulator (`cmd/cardsim`, section 4.7)**

The acquirer opens a chargeback on a captured authorization by control endpoint, standing in for the cardholder's bank, up to the capture and regardless of refunds (decision 7). The merchant accepts, which loses it, or contests with evidence, and the issuer's decision is another control call. Opening and closing each send one event signed in `Cardsim-Signature` to `CARDSIM_WEBHOOK_URL`, so asking the same thing again sends nothing. IDs come from the seed.

**Result:** 4 client tests against the server: a whole chargeback contested and won with exactly two events and every signature valid; a refunded capture charged back in two halves, one lost unanswered and one accepted; 5 refused openings, an unknown chargeback, empty evidence and a fault that leaves the chargeback untouched; the same seed giving the same IDs.

---

**Chargebacks (`GET /v1/chargebacks/{id}`, `/accept`, `/contest`, decisions 7, 26 and 27)**

`chargeback.opened` reaches the inbox at `POST /v1/providers/cardsim/events`, and the worker moves the payment to `charged_back`, stores the chargeback and takes the money back in one transaction: the reais leave the acquirer's receivable, the merchant loses the same share of the payment in USD, fee kept, priced cumulatively like refunds, and the gateway's exchange result absorbs today's rate. The merchant learns of it by `chargeback.opened`, with the chargeback's ID. Accept and contest go to the acquirer first, outside the transaction, and a lost answer is a `503` that changed nothing. A won dispute posts the reversal and sends `chargeback.won`; a lost one sends `chargeback.lost`. Each moves the payment's version. A refunded payment can still be charged back, and the money leaves twice until the dispute is won (decision 7). A chargeback of a capture still being confirmed, or a close before its opening, is retried; the same opening under another event ID is applied once.

**Result:** **F8's criterion:** the GameStore paid by card, paid out on day 30 with US$ 1.94 held, charged back: the balance goes to −US$ 20.00, the next run takes the reserve and leaves −US$ 18.06, and the won dispute brings it back to US$ 1.94, with the acquirer's receivable back at R$ 101.50 and every event matching `ChargebackEvent`. 8 contract tests, plus 6 domain tests and 1 worker test: the above; accepted and lost; refunded then charged back, the payable at −US$ 1.20 until won; two halves at US$ 10.00 each; pending money charged back from pending; 404s, evidence refused and a `503` retried; a replayed opening and an early close; a chargeback before the capture is confirmed, applied after. Pricing and postings by property: chargebacks in any pieces add up to the payment and the whole spread, every posting balanced at any rate. 5 mutants killed. One was a real bug, found by writing the test: the replayed opening was priced before it was recognized, and a whole chargeback seen twice failed as exceeding the payment.

</details>

<details>
<summary><b>Daily reconciliation</b> · 3 tests</summary>

**Comparing a statement with the books (`reconciliation/domain`, section 4.8)**

Each simulator serves `GET /statements/{date}`: the day's money movements in São Paulo time, each with the provider's ID, the gateway's key, the amount and the time, in the same format for all three. `Compare` knows nothing of where either side came from: it pairs lines by kind and by the gateway's key (the provider's ID for a chargeback), each side a multiset, and returns what is missing on either side or has another amount.

**Result:** a property test: every difference planted in a random pair of lists comes back as exactly one divergence, in any order of either side. 6 table cases, among them a same-amount line preferred over the first one and a chargeback and its reversal sharing an ID. 6 mutants killed.

---

**Handling each divergence (`Reconciler.Reconcile`)**

Seven days of a provider's statements against what the gateway recorded, with the amount the ledger posted, plus the money review cases hold. Only the divergences of the day are handled, so each is handled by one day's run. Money the provider received for an attempt of the gateway's is confirmed the way the provider's event would; for a key that names no attempt, it is held in `divergence:BRL` with a case, as in section 4.4, and the case keeps the key so the next run finds that money. A refund the provider completed while the gateway waited for the answer is finished. Anything else (only the gateway has it, another amount, another provider's attempt) is a case with no posting, opened once.

**Result:** 4 worker tests and 1 contract test: a payment whose event was lost confirmed, and the next run finding nothing; two payments for no attempt held once each; what only the gateway has and a different amount opened as cases that move no money, once; another provider's attempt left alone; a refund finished from the simulator's real statement, refused for another amount or provider. 20/20 runs with `-race`. 10 mutants killed.

---

**The daily job (`reconciliation_runs`, decision 6 of F10)**

Every minute the worker reconciles, for each provider, the last 7 closed days not yet done. A worker claims a day with a 10-minute lease: a day that failed or whose worker died is taken again once the lease ends, and a worker that was down for days catches up. Today is left until it closes. `worker reconcile -day YYYY-MM-DD` reconciles a day on demand.

**Result:** four workers on the same days reconcile each once and hold the planted money once; running again, after the lease too, does nothing; a day whose statement failed comes back only after its lease, and the days after it still run. 20/20 runs with `-race`. 6 mutants killed.

</details>

<details>
<summary><b>End to end</b> · 3 tests</summary>

**GameStore walkthrough end to end (`e2e/`, section 4.1)**

The real `api`, `worker` and `pixsim` as processes, on a database migrated by `cmd/migrate` and a merchant created by `cmd/admin`, over HTTP only. The US$ 20.00 charge comes back with the BR Code for R$ 101.50; repeating it with the same key answers the same bytes; the payer pays through the simulator; the worker confirms, posts to the ledger and delivers `payment.paid`, which the merchant checks by its signature and which matches the charge's `GET`. The provider sending the same event again changes nothing. Deviations: the provider takes the creation and never answers, the attempt stays `processing` and the worker's resolver finds it by its key; a merchant that is down does not delay the other, and a manual redelivery reaches it without waiting for the backoff. At the end all three stop on `SIGTERM` and exit cleanly.

**Result:** 3 walkthroughs in ~18 s, in parallel. 5/5 runs with `-race -count=5`. Ledger summing to zero per currency with 1 posting per paid charge, checked by SQL, and the merchant's US$ 19.40 by `GET /v1/balance`. 0 signatures refused by the merchant. 8 wiring mutants killed: the worker without its resolver, inbox or delivery, signing with another key, the API or the worker ignoring `SIGTERM`, the API without the provider inbox or the event routes.

---

**Chaos with a seed (`e2e/`, decisions 7 and 8 of F10)**

The real `api`, `worker` and three simulators as processes. One step at a time, drawn by the seed: PIX, boleto and card payments (approved, declined, captured by hand), half refunds and chargebacks; before about one provider call in three, a `timeout_after` or a 200 ms `delay`; and one PIX charge paid with a key the gateway never gave. With one step at a time, the faults are used in a fixed order, so the seed decides the whole story. At the end the stack settles, `worker reconcile -day` runs for today, and the final state is read without the IDs the gateway generated.

**Result:** **F10's criterion:** the books sum to zero per currency; the reconciliation finds only the planted charge (PIX: 1 divergence, held; card and boleto: none), so every other real on the statements is in the ledger once; seed 42 twice leaves the same state and seed 43 another (cited test 5). 24 operations per run, the three runs side by side in ~66 s with `-race`; the nightly job runs 300. `fail_before` is left out: the gateway only gives up on a call the provider never got after a minute, and the resolver's tests cover it.

---

**The demo (`cmd/demo`, F11)**

`docker compose run --rm demo` against the compose stack, and the same binary against the processes in `e2e/`. It creates the GameStore, registers a webhook endpoint it serves itself, and tells the story with the amounts: a PIX of US$ 20.00 quoted at R$ 101.50, paid at the simulator, `payment.paid` checked by its signature, and the day it settles by the PIX rule (D+1); then a card of US$ 15.00 captured, charged back by the acquirer, contested by the GameStore and won, with the balance at each step. Every wait has a one-minute limit, so a broken step fails the demo instead of hanging it.

**Result:** the balance goes 33.95 → 18.95 → 33.95 USD pending through the dispute. ~14 s against the processes. The CI's `compose` job builds the image, brings up the stack and runs the demo on every pull request: the path of someone who clones.

</details>

CI runs formatting, a strict `golangci-lint`, `go test -race` with coverage and `govulncheck` on every pull request, and a second job brings up `docker compose` and runs the demo. A nightly job reruns everything with `-race -count=100`, the end-to-end tests with `-count=10`, and the chaos script with 300 operations.

## Architecture

A **modular monolith** with hexagonal modules: two binaries from one Go module, and one Postgres that is both the source of truth and the queue. No message broker until load proves one is needed.

```mermaid
flowchart TB
    M[Merchant]
    P[Provider]
    API["cmd/api<br/>receives and records"]
    DB[("PostgreSQL<br/>source of truth and queue")]
    W["cmd/worker<br/>everything that can fail"]
    M -- HTTP --> API
    P -- events --> API
    API --> DB
    W -- SKIP LOCKED --> DB
    W -- HTTP --> P
    W -- signed webhooks --> M
```

The API only receives and records. Anything that can fail or take long runs in the worker, coordinated through Postgres, which holds the ledger, payments, idempotency keys, the inbox and the outbox. The worker applies provider events, resolves `unknown` attempts and refunds, expires payments, settles merchants and delivers webhooks. **No network call ever happens inside a database transaction.**

Each module follows `internal/<module>/{domain,app,adapters}`. The domain imports no HTTP, SQL or SDK, never calls `time.Now()` (it takes a `Clock`), and is tested in isolation.

### Reference flow: GameStore

A US$ 20.00 game; base rate 5.0000 BRL/USD; 1.5% spread gives a quoted rate of 5.0750; the payer pays R$ 101.50; the 3% fee is US$ 0.60. On confirmation the ledger posts, in minor units:

```
BRL:  provider receivables   +10150
      FX (BRL)               -10150      BRL sums to 0
USD:  FX (USD)                -2030      (10150 / 5.0000)
      merchant payable        +1940
      fee revenue               +60
      spread revenue            +30      USD sums to 0
```

End to end, the walkthrough `e2e/` runs against the real binaries:

```mermaid
sequenceDiagram
    participant M as Merchant
    participant A as cmd/api
    participant DB as Postgres
    participant W as cmd/worker
    participant P as pixsim
    M->>A: POST /v1/payments
    A->>DB: key, quote, attempt
    A->>P: create charge
    P-->>A: BR Code
    A-->>M: 201 + BR Code
    Note over P: payer pays
    P->>A: charge.paid
    A->>DB: inbox
    W->>DB: paid + ledger + outbox
    W->>M: payment.paid
    M-->>W: 2xx
```

1. The request carries an `Idempotency-Key`; repeating it answers the same `201`, byte for byte.
2. The attempt is written before the provider is called, and its ID is the provider's idempotency key.
3. The provider's event is signed and stored in the inbox, unique by its external ID, so a duplicate is a no-op.
4. One transaction moves the payment to `paid`, posts the ledger and appends `payment.paid` to the outbox.
5. The webhook is signed with HMAC; a merchant that does not answer `2xx` gets retries with backoff.


## Key decisions

The full list of 58 closed decisions, with reasoning, is in [docs/escopo.md §11](docs/escopo.md) (Portuguese). Below are the choices made while building it, each with the alternative it beat, grouped by area:

<details>
<summary><b>Architecture and testing</b> · 5 decisions</summary>

- **Architecture:** Modular monolith: `api` and `worker` binaries, one Go module.<br>*Discarded:* Microservices with a broker from day one: the cost comes before anyone knows where it hurts.

- **Queue:** Postgres as the queue (`SELECT … FOR UPDATE SKIP LOCKED`) for outbox and jobs.<br>*Discarded:* Kafka: too heavy. NATS: only if load tests show a measured reason.

- **Testing:** Real Postgres in every integration test; determinism only in the provider simulator, controlled by a seed.<br>*Discarded:* Mocked database or fully simulated world: loses real lock and isolation bugs.

- **End-to-end test:** `api`, `worker` and `pixsim` built with `-race` and run as processes, with Postgres from `testcontainers` and a fake merchant checking every signature; all three stop on `SIGTERM` and must exit cleanly.<br>*Discarded:* `docker compose` with a Dockerfile per binary: closer to the deploy, but brings F11 forward and is slower and flakier in CI. Wiring everything in one process: duplicates the `main` wiring and stops testing real config, signals and HTTP.

- **HTTP:** Standard library `net/http` router; hand-written OpenAPI validated in contract tests.<br>*Discarded:* Frameworks and generated code: more to explain, little gained with Go's method+path patterns.

</details>

<details>
<summary><b>Money, FX and ledger</b> · 7 decisions</summary>

- **Money:** `int64` minor units + currency type; decimals in JSON are rejected.<br>*Discarded:* `float64`: `20.099999` becomes the wrong cent.

- **FX rate:** Integer scaled by 10⁸, overflow-free intermediate product, half-even rounding, remainder posted to a rounding account.<br>*Discarded:* A decimal library: a dependency for something small and fully testable.

- **Ledger storage:** Postgres behind a `Ledger` interface; balances derived from entries.<br>*Discarded:* A custom ledger engine (future project); a cached balance, until load tests justify it.

- **Isolation:** `READ COMMITTED` + `SELECT … FOR UPDATE` on accounts, locked in ascending ID order to prevent deadlocks.<br>*Discarded:* `SERIALIZABLE` with retries: more aborted transactions under contention, for a guarantee the row locks already give.

- **Immutability:** Ledger entries can never be updated or deleted, enforced by database triggers; corrections are new transactions.<br>*Discarded:* Editable rows: an audit trail you cannot trust.

- **Chart of accounts:** The gateway's accounts come from a migration with fixed codes; each merchant's payable is created by its first confirmation (`INSERT … ON CONFLICT DO NOTHING`, then `SELECT`)<br>*Discarded:* Creating the payable with the merchant: `merchants` would have to know the ledger, and existing merchants would need a backfill.

- **Quotes:** A `quotes` table owned by `fx` that stores every computed amount, with `CHECK`s for the identities that make the USD legs sum to zero.<br>*Discarded:* Columns inside `payments`: card payments are requoted at authorization (decision 44), so one payment can have more than one quote.

</details>

<details>
<summary><b>Idempotency</b> · 4 decisions</summary>

- **Idempotency:** Keys valid for 24 h; concurrent request with the same key → immediate `409`; successes and 4xx stored, key released on 5xx unless the provider was already called.<br>*Discarded:* Blocking the second request until the first finishes: ties up connections under retries.

- **Key lock:** `INSERT ... ON CONFLICT DO NOTHING` is the lock; the key gets a 30 s lease (`locked_until`) and a `lock_token` only its holder knows.<br>*Discarded:* `SELECT` then `INSERT`: two concurrent requests both get through. `SELECT ... FOR UPDATE` for the whole request: a transaction left open across the provider call.

- **Stored response:** Stored **before** it is sent, with status, body, `Content-Type` and `Location`, on a context the client cannot cancel; a retry gets the original body, including the `request_id` of the request that did the work.<br>*Discarded:* Sending first and storing after: if storing fails, the client saw the 201 and the retry gets `409` for 30 s.

- **Resuming after a crash:** Recovery points: the key stores how far the request got and what it created, written in the same transaction as that work; a retry continues from there.<br>*Discarded:* Redoing everything: a request that died after creating the payment yields a second payment, and in F5 a second provider attempt.

</details>

<details>
<summary><b>Public API</b> · 6 decisions</summary>

- **API errors:** RFC 9457 `application/problem+json` with a stable `code` and the `request_id` that also appears in the logs.<br>*Discarded:* A custom error format: one more thing for clients to learn, and a bare status code cannot tell causes apart.

- **API keys:** `sk_test_`/`sk_live_` + public lookup + 256-bit secret; only the SHA-256 is stored and compared in constant time; the key is printed once by `cmd/admin`<br>*Discarded:* bcrypt/argon2: built for guessable passwords, they would add tens of milliseconds to every request without protecting a random 256-bit secret any better.

- **Public IDs:** `pay_` + the UUIDv7 in Crockford base32 (26 characters), so IDs sort by creation time as plain strings.<br>*Discarded:* Hex: 32 characters for the same information. Exposing the raw UUID: no type prefix to catch a payment ID passed where a refund ID belongs.

- **Public IDs across modules:** `internal/platform/publicid`, pure, used by `payments` and `webhooks`<br>*Discarded:* Copying the alphabet into each module: two encodings to keep equal forever.

- **Pagination:** Keyset on `(merchant_id, id)`, newest first, with an opaque cursor in `next_cursor` (`null` on the last page); `limit` from 1 to 100, default 20.<br>*Discarded:* `OFFSET`: slower with every page, and skips or repeats rows when new payments arrive. `starting_after=pay_…`: works, but ties the cursor format to the ID.

- **Create response:** Always `201` once the payment exists, with `pix.status` `ready` (with the BR Code), `processing` or `failed`, and `boleto.status` with the same three; internal attempt statuses never leak.<br>*Discarded:* A `5xx` while `unknown`: clients would retry a request that already did its work. A `422` on refusal: it would hide a payment that exists.

</details>

<details>
<summary><b>Providers, PIX, card and boleto</b> · 22 decisions</summary>

- **Choosing the provider:** By method for a new payment; by the name stored with the attempt or refund afterwards, so money goes back where it came in.<br>*Discarded:* One provider for everything: no card or boleto. Routing every call by method: a route changed in a deploy would send a refund to a provider that never had the money.

- **Card simulator:** A separate acquirer, `cmd/cardsim`, with faults, seed and signing shared through `internal/providers/simkit`.<br>*Discarded:* Card endpoints inside `pixsim`: PIX and card would look like one provider, and the routing would never be tested.

- **Card data:** Only a fictitious token, kept with the attempt under a `CHECK` that refuses anything else, so a retried request can finish a create that died before calling the acquirer.<br>*Discarded:* A card number: PCI DSS scope (section 7). Not storing the token: a crashed create could never be completed.

- **Card answer on create:** Authorized and captured at once (decision 18), so the `201` is reread and shows the payment `paid`.<br>*Discarded:* Answering `pending` and confirming later: the merchant would wait for news the gateway already has.

- **Card quote:** 30 minutes when capture is automatic, since the quote is used as the payment is created; the authorization's 7 days when the merchant captures later, since the rate is locked at the authorization.<br>*Discarded:* 7 days for every card: a declined card would sit pending for a week.

- **Partial capture:** The captured part priced again at the locked rate, stored as its own quote that the books, refunds and balance read.<br>*Discarded:* Scaling the original quote's lines by the captured share: a fee and a spread that no longer come from their own formula, and a payer amount that may differ by a cent from the acquirer's. Repricing at today's rate: the payer would pay a rate never shown.

- **Capture idempotency:** None needed: an authorization is captured once, so the same capture is answered as it is and another amount is a `409`, even after a capture the acquirer may not have received.<br>*Discarded:* An Idempotency-Key: a second key with another amount could capture twice.

- **Boleto confirmation:** The simulator's signed `boleto.paid`, through the same inbox as PIX; the CNAB 240 return file was left out of F10, whose reconciliation reads the simulators' statements instead.<br>*Discarded:* Waiting for CNAB: F8's boleto would depend on a later parser, and a real bank announces the payment before the next day's file anyway.

- **Boleto due date:** Three calendar days in `America/Sao_Paulo` (decision 44), received until the end of the first banking day on or after it; the quote lasts until then, and the date is stored with the attempt.<br>*Discarded:* Computing the due date on every call: a create resumed after midnight would ask for another boleto with the same key. Three business days: decision 44 says calendar days, and the banking day already enters the last day to receive.

- **Lapsed boleto:** The simulator shows it `expired` and the adapter reads it as `accepted`: nobody paid, and the payment expires by its own quote once the job has asked the bank.<br>*Discarded:* `rejected`: the state machine does not leave `accepted` on a refusal, and a refusal would free the payment for a second attempt.

- **Payer data:** On a boleto only, as the token is on a card only; the CPF checked by its digits, stored bare because the bank needs it, and masked in `String`, `slog` and responses, none of which ever shows it back.<br>*Discarded:* A payer on every payment: personal data with no use for PIX or card. Echoing the payer in the response: the merchant already has it, and the CPF would be in every response log.

- **Another provider's event:** An event confirms only an attempt of the provider that sent it, checked under the payment's lock.<br>*Discarded:* Trusting the attempt ID: with two simulators, an authentic event of one could confirm an attempt of the other.

- **Ambiguous provider result:** Attempt goes to `unknown` and is resolved by actively polling the provider (`GetAttempt` with backoff), with events and reconciliation as a safety net.<br>*Discarded:* Treating timeouts as failures: the provider may have charged the payer.

- **Provider attempts:** Written as `created` before the provider is called, one live attempt per payment enforced by a partial unique index, every change through the state machine under a row lock.<br>*Discarded:* Calling first and writing after: a crash mid-call leaves no trace of a payer who may have been charged. Uniqueness checked only in code: two concurrent requests both pass.

- **Provider errors:** A refusal is a result (`Rejected`); any error, and any result the adapter left unset, moves the attempt to `unknown`<br>*Discarded:* An `ErrAmbiguous` sentinel from the adapter: an error it forgets to wrap would read as a definitive failure, the expensive mistake.

- **Writes around the provider call:** Payment, quote, attempt `created` and the recovery point in one transaction; `sent` written before the call; the answer written after it. `created` therefore proves the provider was never called, and only `sent` is ambiguous.<br>*Discarded:* Not writing `sent`: a crash after the first transaction could not tell "never called" from "maybe called", so every crash would end in `unknown`

- **Claiming provider lookups:** `UPDATE … SET next_check_at = now + 1 min WHERE id IN (SELECT … FOR UPDATE SKIP LOCKED)`: the row lock lasts only that short transaction, and the pushed `next_check_at` is a lease that keeps other workers away during the call.<br>*Discarded:* `FOR UPDATE` held during the lookup: a transaction open across the network. A separate `claimed_until` column: two columns for what `next_check_at` already does.

- **Provider says not found:** Treated as ambiguous for 1 minute after the attempt was created, then `rejected`: our create gave up after 5 s, so nothing can still arrive.<br>*Discarded:* Calling create again with the same key: the provider would dedupe it, but a QR code appearing minutes after the merchant saw `processing` is behavior nobody asked for.

- **Provider simulator:** Real HTTP server with charges in memory; every random choice comes from a generator seeded by the seed and the idempotency key; failures scripted through `/control/faults`. The client speaks only the simulator's types and a ~60-line translation in `payments` adapts them to the port.<br>*Discarded:* An in-process fake: never produces a real timeout. One generator for the whole simulator: concurrent requests would draw in a different order each run. The adapter inside `providers` returning `payments` types: inverts the module dependency of the scope.

- **PIX txid:** Chosen by the simulator (25 characters from the seed) and returned with the charge; our attempt ID travels as the `Idempotency-Key`<br>*Discarded:* Packing the 26-character attempt ID into 25 by dropping the UUIDv7 version and variant bits: lossless, but cleverness with nothing gained.

- **Simulator event signature:** `Pixsim-Signature: t=<unix>,v1=<hex HMAC-SHA256(secret, t.body)>`, compared with `hmac.Equal` and a 5-minute tolerance; several `v1` allowed, so a secret can rotate.<br>*Discarded:* Signing only the body: a captured delivery replays forever. A plain shared token in a header: proves nothing about the body. mTLS: a real provider's job, not the simulator's.

- **BR Code:** Own codec, printable ASCII only; the simulator issues the static code with amount and txid.<br>*Discarded:* A third-party library: a dependency for ~380 lines testable against the manual. A dynamic code in the simulator: the amount lives in a signed JSON (JWS) hosted at the URL, unrelated to what F5 proves.

</details>

<details>
<summary><b>Incoming events and confirmation</b> · 10 decisions</summary>

- **Incoming events:** Inbox table: the webhook handler only stores and returns `200`; the worker processes, tolerating duplicates and reordering.<br>*Discarded:* Processing inside the handler: provider retries and slow handlers turn into double processing.

- **Applying an event:** The handler runs inside the transaction that claimed the event and records its status.<br>*Discarded:* Two transactions: a crash between them leaves the effect applied with the event still pending, or the event done without its effect.

- **Event that cannot be applied:** For an attempt not stored yet: retried with backoff, `failed` at 24 h (decision 52). Contradicting what the gateway knows: `failed` at once, for a person. A type nobody handles: `ignored`<br>*Discarded:* Retrying everything: a wrong amount would be retried for a day and never become right.

- **Redelivery with another body:** The first body stays as the proof; the second goes only to the log.<br>*Discarded:* Overwriting: the stored proof would stop being what the provider signed first.

- **Payment lock for events:** `FOR NO KEY UPDATE`, the lock a plain `UPDATE` takes: events of one payment run one at a time.<br>*Discarded:* `FOR UPDATE`: it also waits for the `KEY SHARE` held by every insert of a row that points at the payment, such as a new attempt.

- **Where confirmation happens:** In the one function that moves attempts, when one becomes `succeeded`: the provider's event and the resolver both pass through it.<br>*Discarded:* Only in the event handler: a payment the resolver found would never reach the ledger.

- **Lock order:** Payment (`FOR NO KEY UPDATE`), then attempt, then ledger accounts ascending, on every path.<br>*Discarded:* Each path in its own order: the event locked payment then attempt, and the resolver, once confirming, would lock attempt then payment, which is a deadlock waiting to happen.

- **Invariant 7, one confirmation per payment:** Two barriers: the payment's lock with the state machine (`paid` + `confirm` is a no-op), and a unique ledger reference `payment:<id>:confirm` that refuses a second post if the first barrier ever breaks.<br>*Discarded:* Trusting the status alone: one bug in a path and the money is counted twice.

- **Who owns the review queue:** The `reconciliation` module (section 2.1), which also runs the daily reconciliation since F10; payments opens cases through a port bridged in `cmd`, as with the outbox.<br>*Discarded:* A table in payments: F10's reconciliation would need a second queue or a data migration. The inbox's `failed`: it holds no ledger posting, and the expiry job has no event.

- **Payment of a closed payment:** Late or another amount: the attempt `succeeded` (the provider has the money), the payment untouched, the money held.<br>*Discarded:* Undoing everything, as before: the money stayed outside the ledger, and a late event left the attempt halfway.

</details>

<details>
<summary><b>Outbox and event format</b> · 5 decisions</summary>

- **Outbox across modules:** `payments` declares an `Outbox` port that takes its transaction; `cmd` plugs in the `events` adapter through a bridge, so neither module imports the other.<br>*Discarded:* `payments` importing `events`: a dependency the module table does not have. An outbox table owned by `payments`: the outbox belongs to `events`

- **Stored event:** The whole event as `bytea`, exactly the bytes every delivery sends, with an ID from `uuidv7()` taken in the same transaction before the JSON is built.<br>*Discarded:* `jsonb`: reorders keys and drops whitespace, so the stored bytes stop being the signed ones. Building the envelope at delivery: the row stops being the event.

- **Event ordering:** `payments.version`, bumped by the `UPDATE` that changes the status and sent as `payment_version`<br>*Discarded:* Counting events: not every transition will emit one.

- **Event format:** `PaymentEvent` in `components/schemas` of the OpenAPI 3.0 document, checked against the stored bytes in a contract test.<br>*Discarded:* OpenAPI 3.1 `webhooks:`: the right place, but it means migrating the whole document and its validator for one schema.

- **Refund events:** `refund.succeeded` and `refund.failed`, a `RefundEvent` schema of their own with the `refund_id`, the amount, the payment's status and its `payment_version`: one event per fact, ordered with the payment's events.<br>*Discarded:* Only `payment.refunded` and `payment.partially_refunded` in the existing schema: says nothing about which refund it was, and nothing at all when a refund fails.

</details>

<details>
<summary><b>Expiry and cancel</b> · 5 decisions</summary>

- **What counts as late:** The provider's `paid_at` at or after the quote's `expires_at`; a payment without `paid_at` is late.<br>*Discarded:* Our processing time: the payer would pay for our queue's delay and the provider's.

- **Expiring:** Only after the provider says the payment was not made on time; no answer, no decision, retried after the lease.<br>*Discarded:* A grace period: makes the race rarer, not impossible. Allowing `expired → paid`: section 3.4 makes `expired` terminal, and the merchant would see a payment come back.

- **"Every read checks the deadline" (section 4.6):** Applied to decisions: `confirm` checks `paid_at` even if the job has not run; `GET` shows the stored status.<br>*Discarded:* Computing `expired` on read: a payment shown as expired could still be confirmed by the job, a status going backwards.

- **Expiry queue:** `payments.expiry_check_at`, set to the quote's expiry and used as the lease, with a `CHECK` that only a pending payment has one and a partial index.<br>*Discarded:* A `JOIN` with `quotes` every round: no index serves it, so it reads every pending payment.

- **Cancel:** Asks the provider first, as expiry does, and decides nothing without an answer; no `Idempotency-Key`, because a repeated cancel is a no-op by the state machine.<br>*Discarded:* Canceling locally only: a payer who paid on time, with the event still on its way, would have the money held for review instead of the payment confirmed.

</details>

<details>
<summary><b>Refunds and settlement</b> · 11 decisions</summary>

- **Who pays for a refund:** The merchant pays the refunded amount whole: the fee is not returned. The spread returns its share, since the payer gets back the same reais they paid. The gateway absorbs the rate change in the exchange result.<br>*Discarded:* Returning the fee too: generous, but fee revenue could go negative. Paying back reais at today's rate: the payer would get a different amount than they paid.

- **When a refund leaves the balance:** On creation, held on "refunds in flight" until the provider answers; the payment changes status only on success.<br>*Discarded:* Posting only on success: the merchant's balance would count money already on its way back.

- **Refund left `created`:** The worker sends it: the provider certainly got nothing, and the merchant already asked for the refund.<br>*Discarded:* Waiting for a retried request: without one, the hold would stay forever. Rejecting it: the merchant would have to ask again for a refund nothing prevented.

- **Where settlement lives:** In `payments`, as refunds do: it reads payments, refunds and the merchant's payable in one transaction.<br>*Discarded:* Section 2.1's `settlement` module with a port into `payments`: the port would need the transaction and to lock another module's payments.

- **Available:** Derived: the payable's balance less what is pending, computed under the payable's lock.<br>*Discarded:* An available account per merchant and a job moving each payment on the right day: one more posting per payment and one more job to get the date wrong.

- **Start of availability:** When the confirmation is posted (`paid_at`)<br>*Discarded:* The provider's paid time: late news would make money available in a cutoff that already ran.

- **Negative available:** Takes the reserve first, then pays nothing and waits for more money (decision 27); the available payments go into the first positive payout.<br>*Discarded:* A negative payout: the gateway would be charging the merchant's bank account, which it has no mandate to do.

- **Rolling reserve:** 10% of each payout, held 90 days per settlement and released by a later run, oldest first (decision 26).<br>*Discarded:* A fixed reserve per merchant: a new merchant with little volume would have nothing held, and a large one too much. Holding a share of each payment: one more posting per payment for the same money.

- **Debt limit:** Payable plus reserve below −US$ 1,000.00 refuses new payments, read without the payable's lock at creation.<br>*Discarded:* Locking the payable: every create would queue behind the merchant's confirmations, for a limit that a payment in flight cannot cross alone.

- **Reading the balance:** One `REPEATABLE READ` snapshot, without the payable's lock: the payable and what is pending agree because every change to what is pending posts to the payable in the same transaction.<br>*Discarded:* The payable's lock, as the settlement takes it: every `GET /v1/balance` would queue behind confirmations and refunds of the merchant.

- **Payments of a settlement:** Their IDs inline in `GET /v1/settlements`, one settlement a day per merchant, read through a partial index on `payments.settlement_id`<br>*Discarded:* A `?settlement=` filter on `GET /v1/payments` or a `GET /v1/settlements/{id}`: one more endpoint, worth it when a day holds more payments than a page should carry.

</details>

<details>
<summary><b>Chargebacks</b> · 5 decisions</summary>

- **What the merchant loses:** The BRL share of the payment the bank took back, in USD, fee kept, priced cumulatively as refunds; the gateway's exchange result takes today's rate.<br>*Discarded:* Converting the reais at today's rate: the merchant would win or lose on the exchange of a dispute it did not choose. A chargeback fee: none is in the scope, and it would hide the movement under test.

- **When the money leaves:** At opening; a won dispute posts the reversal.<br>*Discarded:* Only when lost: the acquirer already took the reais, and the merchant's balance would count money that is gone.

- **Refunded, then charged back:** The money leaves again (decision 7), and the merchant contests with the refund as evidence.<br>*Discarded:* Netting the refund: the acquirer takes the reais regardless, and the books would disagree with the bank until the dispute ends.

- **Event before what it names:** A chargeback of a payment still pending, or a close before its opening, is retried as an event for an attempt not stored yet. The same opening under another event ID is applied once, by the acquirer's chargeback ID.<br>*Discarded:* Failing them: the order of webhooks is not guaranteed, and each would wait for a person.

- **Answering the dispute:** Accept and contest call the acquirer first and store its answer; the same answer again returns the chargeback as it is.<br>*Discarded:* Storing first and sending later: the merchant would be told a contest went through that the acquirer could still refuse.

</details>

<details>
<summary><b>Webhooks</b> · 17 decisions</summary>

- **Webhook secret:** Derived: HMAC-SHA256 of a key kept outside the database (`WEBHOOK_SIGNING_KEY`), the merchant and a version; the table stores only the version.<br>*Discarded:* Hashed, as section 6 asked for API keys: a hash cannot sign, and HMAC needs the secret. Encrypted with AES-GCM: the same protection against a dump, with nonce, format and key rotation to get right.

- **Retrying a rotation:** Conditional on the current version (`rotate_secret_from`): a repeat finds it rotated and shows the same secret.<br>*Discarded:* `Idempotency-Key`: the stored answer would hold the secret in plaintext.

- **Where the SSRF rule lives:** `internal/platform/ssrf`, pure, used by merchants at registration and by webhooks at delivery.<br>*Discarded:* Inside webhooks: merchants would import it, against section 2.1. A copy in each: two lists of blocked ranges to keep equal.

- **Development webhooks:** `WEBHOOK_ALLOW_PRIVATE` opens private ranges and `http`, never cloud metadata; false by default.<br>*Discarded:* Opening them for `sk_test_` keys: a test key in production would open SSRF.

- **Delivering with no open transaction:** Lease: the claim pushes `next_attempt_at` past the timeout + 1 min and bumps `attempts`, commits, and the POST runs outside any transaction; a worker that dies lets the lease run out.<br>*Discarded:* Holding `FOR UPDATE` through the POST: network inside the transaction (section 4.3), and a slow merchant holds pool connections.

- **Delivery state:** Columns on `outbox_events`: one endpoint per merchant, one delivery per event.<br>*Discarded:* A `webhook_deliveries` table per attempt: history, but one more join and one more cleanup, only worth it with several endpoints per merchant.

- **Per-merchant isolation:** In the claim: up to 4 events per merchant per round, merchants with the oldest event first, and a short timeout. The cap holds per worker process.<br>*Discarded:* An in-memory semaphore: also per process, and one merchant could take a whole batch. A global cap: counting live leases under a per-merchant advisory lock, once there is more than one worker.

- **Webhook signature:** `Webhook-Signature: t=…,v1=…`, the simulator's scheme; after a rotation, a second `v1` from the previous secret for 24 h.<br>*Discarded:* Standard Webhooks: a second scheme in the repository. Signing with the previous one until the next rotation: a leaked secret that was rotated away would keep working.

- **SSRF at delivery:** `net.Dialer.Control` checks the resolved IP right before `connect`, with no proxy and no redirects followed.<br>*Discarded:* Resolve, check, then connect: the name can answer another IP between the two lookups.

- **Delivery order:** Not guaranteed: the merchant ignores a `payment_version` below the last one (section 4.5)<br>*Discarded:* Serializing per payment: less throughput for a guarantee the envelope already gives.

- **Notice of a disabled destination:** The state in `GET /v1/webhook-endpoint` and an `ERROR` log line.<br>*Discarded:* A webhook: that is the destination that is down. A notifications table: a channel nobody reads until there is a dashboard. Email: out of scope.

- **Turning back on:** Any `PUT` of the endpoint, with the same URL too; if it is not fixed, it is disabled again in 72 h.<br>*Discarded:* An `enabled` field: a merchant who only fixes the URL forgets it and stays disabled.

- **Events of a disabled destination:** Wait with the usual backoff, with no post, and fail at 72 h.<br>*Discarded:* Filtering in the claim with a `JOIN` on `webhook_endpoints`: the `events` query would read a `merchants` table (section 2.1)

- **Retention of delivered events:** 30 days, in batches of 1,000 per hour; never `failed` or `pending`<br>*Discarded:* 7 days: fewer rows, less time for the merchant to look back.

- **Manual redelivery:** A new 72 h window with the backoff restarting at 30 s, kept in `window_started_at` and `window_first_attempt`; `attempts` never goes back.<br>*Discarded:* One attempt only: a separate mode in the worker, and the merchant would have to watch the result. Resetting `attempts`: an old claim would match the fence again.

- **Redelivery with nowhere to go:** `409` with the reason's code; the merchant does a `PUT` first.<br>*Discarded:* Accepting and waiting: a `202` for an event that would fail again in 72 h.

- **Body in the event list:** The stored bytes, in `event`, next to `delivery`<br>*Discarded:* Flattening event and delivery into one object: it would no longer be the signed body.

</details>

<details>
<summary><b>Daily reconciliation and chaos</b> · 5 decisions</summary>

- **The statement:** `GET /statements/{date}` on each simulator, one format in `simkit`: the day's money movements in São Paulo time, each with the provider's ID, the gateway's key, amount and time, ordered so the same seed gives the same statement.<br>*Discarded:* A statement per kind of operation: three formats to compare the same thing.

- **The gateway's side:** Attempts, refunds and chargebacks dated by when the gateway learned them, each with the amount its ledger posting moved on the provider's receivable, plus the money review cases hold.<br>*Discarded:* Dating by the ledger's `created_at`: it is the database's clock, not the one the domain records facts by.

- **Money only the provider has:** With the gateway's key on an attempt, the usual confirmation, which already handles lateness and another amount; with no attempt, held in `divergence:BRL` with a case, as in section 4.4.<br>*Discarded:* Always a case: the resolver already confirms on the same evidence, and a person would redo it by hand.

- **What only a person decides:** Missing at the provider, another amount, another provider's attempt: a case, with no posting; the posting already made stays.<br>*Discarded:* Correcting the ledger automatically: which side is right is not something the statement shows.

- **Faults in the chaos script:** Scheduled right before the call that uses them, one step at a time, so the order, and so the story, comes from the seed; `timeout_after` and `delay` only.<br>*Discarded:* Concurrent steps: which request gets a fault would depend on timing. `fail_before`: a minute of `NotFoundGrace` per fault.

</details>

<details>
<summary><b>Local delivery</b> · 4 decisions</summary>

- **One image:** A multi-stage `Dockerfile` builds every binary into one rootless `distroless/static` image; each compose service picks its command, and only `migrate` builds it.<br>*Discarded:* An image per binary: eight builds of one module. `build:` on every service: they raced to build the same tag on the first `up`.

- **Waiting for the services:** The demo waits for `/readyz` and the simulators' `/healthz`; the Go services have no compose healthcheck.<br>*Discarded:* A `-healthcheck` mode in each binary: production code for a demo's infrastructure, since a distroless image has no `curl`.

- **The demo in Go:** `cmd/demo`, in the same image, so the second command needs nothing on the host but Docker, and the demo has a test.<br>*Discarded:* A shell script with `curl` and `jq`: shorter, but needs tools on the host and has no test.

- **Settlement in the demo:** The pending balance and the day it settles by the PIX rule (D+1).<br>*Discarded:* A configurable settlement delay set to zero for the demo: it would show the payout, through production configuration that only the demo needs.

</details>

## Status

The plan is split into phases, each with a written definition of done ([docs/escopo.md §12](docs/escopo.md)).

| Phase | Delivers | Status |
|---|---|---|
| F0 | Skeleton, CI, real Postgres in tests | ✅ done |
| F1 | Money and FX: half-even conversion, quotes with spread, fee and expiry | ✅ done |
| F2 | Ledger with invariants under concurrency | ✅ done |
| F3 | Payments, state machine, base API | ✅ done |
| F4 | Idempotency | ✅ done |
| F5 | Simulated PIX, inbox, confirmation | ✅ done |
| F6 | Outbox and webhooks; GameStore end to end over HTTP | ✅ done |
| F7 | Refunds and settlement | ✅ done |
| F8 | Card, boleto, chargebacks | ✅ done |
| F9 | Polish: less code, comments only for the why, no behavior change ([plan](docs/polimento.md)) | ✅ done |
| F10 | Chaos test across the three simulators and daily reconciliation ([plan](docs/f10.md)) | ✅ done |
| F11 | `docker compose up` with everything and a GameStore demo script ([plan](docs/f11.md)) | ✅ done |

With F11, every phase of the plan is done: a clean clone runs the demo with the two commands of the [Quick start](#quick-start).

On 14 November 2026, after F8, the Efí sandbox, the public deploy, load tests and the payer page were cut (decision 58): F0–F8 already prove the four axes, and the time goes into a smaller codebase instead.

What F9 changed, in lines (`wc -l`, without the code sqlc generates):

| | Before | After |
|---|---|---|
| Production Go | 17,147 | 16,427 |
| Comments in production Go | 1,630 | 924 |
| Test Go | 19,009 | 18,894 |
| `api/openapi.yaml` | 1,505 | 1,493 |

## Run it locally

The quickest way is the [Quick start](#quick-start): `docker compose up -d` builds one image with every binary and starts the API on port 8080 and the simulators on 8090–8092, all bound to `127.0.0.1`. To run each part on its own, you need [mise](https://mise.jdx.dev) and Docker. mise pins Go, `golangci-lint`, `sqlc` and `govulncheck` to the exact versions CI uses.

```sh
mise install          # toolchain at pinned versions
mise run test         # go test -race ./..., Postgres in containers

mise run db           # local Postgres via docker compose
mise run migrate      # apply embedded migrations
# create a merchant; prints its API key once
go run ./cmd/admin merchant create -name GameStore -mode test
mise run pixsim       # PIX simulator on :8090
mise run cardsim      # card simulator on :8091
mise run boletosim    # boleto simulator on :8092
go run ./cmd/api      # API on :8080
go run ./cmd/worker   # background jobs
# reconcile one day now, as the daily job does for closed days
go run ./cmd/worker reconcile -day 2026-11-15
```

The worker applies inbox events, resolves `unknown` attempts and refunds, expires payments, settles merchants and delivers webhooks; every hour it deletes idempotency keys older than 24 h and events delivered more than 30 days ago. The variables they read; `mise.toml` sets the development ones:

| Variable | Default | What it does |
|---|---|---|
| `DATABASE_URL` | set by mise | The Postgres of `mise run db` |
| `FX_USD_BRL_RATE` | `5.0000` | Base rate (API, worker) |
| `PIXSIM_URL` | `http://localhost:8090` | Where the PIX simulator is |
| `CARDSIM_URL` | `http://localhost:8091` | Where the card simulator is |
| `BOLETOSIM_URL` | `http://localhost:8092` | Where the boleto simulator is |
| `PIXSIM_SEED`, `CARDSIM_SEED`, `BOLETOSIM_SEED` | `1` | Seed of every random choice of each simulator |
| `PIXSIM_WEBHOOK_URL` | set by mise | Where the simulator posts its events: the API's inbox |
| `PIXSIM_WEBHOOK_SECRET` | set by mise | Signs the simulator's events; the API verifies them |
| `BOLETOSIM_WEBHOOK_URL`, `BOLETOSIM_WEBHOOK_SECRET` | set by mise | The same for the boleto simulator; the API requires the secret |
| `CARDSIM_WEBHOOK_URL`, `CARDSIM_WEBHOOK_SECRET` | set by mise | The same for the card simulator's chargebacks; the API requires the secret |
| `WEBHOOK_SIGNING_KEY` | set by mise | 32 bytes or more; derives each merchant's webhook secret |
| `WEBHOOK_TIMEOUT` | `5s` | Timeout of each webhook post |
| `WEBHOOK_ALLOW_PRIVATE` | `false` (mise: `true`) | Lets webhooks reach `http` and private networks, never cloud metadata |

Creating the GameStore payment with the key `cmd/admin` printed:

```sh
KEY=sk_test_...   # the key cmd/admin printed
API=localhost:8080

curl -s $API/v1/payments -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: order-123" \
  -d '{"amount": {"value": 2000, "currency": "USD"},
       "method": "pix", "reference": "order-123"}'
# 201 pending: the payer pays R$ 101.50, fee 60, net 1940,
# quote valid 30 min, pix.br_code "000201...6304XXXX".
# Again with the same key: the same 201, Idempotent-Replayed: true.

curl -s $API/v1/payments/pay_... -H "Authorization: Bearer $KEY"
curl -s "$API/v1/payments?limit=20&status=pending" \
  -H "Authorization: Bearer $KEY"
# 200 {"data": [...], "next_cursor": null}
# Pass next_cursor as ?cursor= for the next page.
```

A card authorized now and captured in part later (needs `mise run cardsim`):

```sh
curl -s $API/v1/payments -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: order-124" \
  -d '{"amount": {"value": 2000, "currency": "USD"},
       "method": "card", "reference": "order-124",
       "card": {"token": "tok_approved",
                "capture_method": "manual"}}'
# 201 pending, card.status "authorized", quote valid 7 days

curl -s -X POST $API/v1/payments/pay_.../capture \
  -H "Authorization: Bearer $KEY" \
  -d '{"amount": {"value": 1500, "currency": "USD"}}'
# 200 paid, card.captured: R$ 76.12, fee 45, net 1455
# No body captures it all; without capture_method it is
# captured on create and the 201 is already paid.
```

A boleto for the same GameStore (needs `mise run boletosim`):

```sh
curl -s $API/v1/payments -H "Authorization: Bearer $KEY" \
  -H "Idempotency-Key: order-125" \
  -d '{"amount": {"value": 2000, "currency": "USD"},
       "method": "boleto", "reference": "order-125",
       "payer": {"name": "Maria Silva", "document": "529.982.247-25"}}'
# 201 pending, boleto.status "ready", a 44-digit barcode,
# digitable_line "99990.00129 ...", due_date three days on;
# the quote lasts until the end of the bank's last day.
```

Balance and payouts:

```sh
curl -s $API/v1/balance -H "Authorization: Bearer $KEY"
# 200 available, pending and reserved, in USD
curl -s "$API/v1/settlements?limit=20" -H "Authorization: Bearer $KEY"
# 200 payouts by cutoff date, each with its payments
```

Chargebacks, with the ID from `chargeback.opened`:

```sh
curl -s $API/v1/chargebacks/cbk_... -H "Authorization: Bearer $KEY"
# 200 needs_response, payer_amount in BRL, amount lost in USD
curl -s -X POST $API/v1/chargebacks/cbk_.../contest -H "Authorization: Bearer $KEY" \
  -H "Content-Type: application/json" -d '{"evidence": "delivered: tracking BR123"}'
# 200 under_review; chargeback.won or chargeback.lost when the issuer decides
curl -s -X POST $API/v1/chargebacks/cbk_.../accept -H "Authorization: Bearer $KEY"
# 200 lost at once; 409 chargeback_closed once contested
```

Registering the webhook endpoint, and rotating its secret:

```sh
curl -s -X PUT $API/v1/webhook-endpoint -H "Authorization: Bearer $KEY" \
  -d '{"url": "https://gamestore.example/hooks"}'
# 200 secret_version 1 and secret "whsec_...", shown only here

curl -s -X PUT $API/v1/webhook-endpoint -H "Authorization: Bearer $KEY" \
  -d '{"url": "https://gamestore.example/hooks", "rotate_secret_from": 1}'
# 200 secret_version 2 and its secret; repeating shows it, no new rotation

curl -s $API/v1/webhook-endpoint -H "Authorization: Bearer $KEY"
# 200 without the secret; disabled_at and disabled_reason after 72 h
# of only failures (any PUT turns it back on)

curl -s "$API/v1/events?delivery_status=failed" \
  -H "Authorization: Bearer $KEY"
# 200 each event with the body sent and where its delivery stands

curl -s -X POST $API/v1/events/evt_.../redeliver \
  -H "Authorization: Bearer $KEY"
# 202 pending, due now, in a new 72 h window
```

With the worker running, every event reaches that URL with `Webhook-Signature: t=<unix>,v1=<hex>`, the HMAC-SHA256 of `t`, a dot and the body, keyed by the secret; the `PaymentEvent` schema in the OpenAPI describes the body and how to verify it.

The contract is [api/openapi.yaml](api/openapi.yaml).

Other tasks: `mise run lint`, `mise run fmt`, `mise run vuln`, `mise run e2e` (only the end-to-end tests: GameStore and the short chaos script), `mise run test:stress` (the nightly `-count=100` run), `mise run gen` (`sqlc`).

## Repository layout

```
cmd/
  api/        HTTP server
  worker/     background jobs, with graceful shutdown
  migrate/    applies embedded goose migrations
  admin/      operator tasks: create a merchant and its API key, revoke a key
  demo/       the GameStore story against a running gateway
  pixsim/     PIX simulator: seed, faults, "pay now", events
  cardsim/    card simulator: tokens, authorization, capture
  boletosim/  boleto simulator: registration, due date, "pay now"
internal/
  money/      Money and Currency in minor units
  brcode/     PIX BR Code: EMV TLV and CRC16, encoder and parser
  boleto/     Febraban barcode and linha digitável, check digits, due date factor
  calendar/   dates and banking days, movable holidays from Easter
  cpf/        CPF validation and masking
  fx/         rates, conversion, quotes
  ledger/     double-entry transactions, invariants, Postgres adapter
  merchants/  merchants, API keys, webhook endpoints
  payments/   payments, attempts, refunds, settlement
  events/     provider inbox and the outbox
  webhooks/   webhook delivery to merchants
  reconciliation/ daily reconciliation against the statements, review cases
  providers/  simulators (pixsim, cardsim, boletosim) and what they share (simkit)
  idempotency/ idempotency keys: lock, lease, recovery points
  platform/   config, logs, clock, Postgres, HTTP, SSRF, public IDs
api/          openapi.yaml, the hand-written contract
migrations/   SQL migrations, embedded in the binaries
docs/         scope, rules and decisions (Portuguese)
e2e/          GameStore walkthrough, the chaos script and the demo, against the real binaries
Dockerfile    one image with every binary
compose.yaml  the whole gateway locally, and the demo
```

## Stack

Go 1.27 · PostgreSQL 18 · `pgx/v5` · `sqlc` · `goose` (as a library) · `log/slog` · `testcontainers-go` · `rapid` (property-based tests) · `golangci-lint` v2 + `gofumpt` · GitHub Actions · mise

## Regulation

Not legal advice. Processing third-party money in Brazil requires Central Bank authorization as a payment institution, which is why no real money is involved here. Taxes (IOF) are at most a configurable fee. KYC/AML is out of scope, though the ledger and inbox audit trails show where it would plug in. No card numbers are stored, only fictitious tokens, and personal data is masked in logs (LGPD minimization).

## Author

**Felipe Gegembauer**, Bachelor of Software Engineering student at PUCPR (Curitiba, Brazil).

[GitHub](https://github.com/gengibrepower) · [LinkedIn](https://www.linkedin.com/in/felipe-gegembauer/)

## License

MIT. See [LICENSE](LICENSE).
