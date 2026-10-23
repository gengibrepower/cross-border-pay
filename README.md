# Cross-border Pay — a cross-border payment gateway in Go

[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

🇧🇷 [Leia em português](README.pt-BR.md)

A foreign merchant charges in US dollars; a payer in Brazil pays in reais by PIX, boleto or card; the system converts, records everything in a double-entry ledger and pays the merchant out in dollars. The money on record always matches the money that exists, even under failures, duplicates and out-of-order events.

> **This is a personal learning project, not a real gateway.** Simulated money, no customers, simulated providers. It exists to practice the engineering of correct financial systems.

## Status

Scope and decisions are closed; code starts at F0. The full plan (Portuguese) is in [docs/escopo.md](docs/escopo.md).

| Phase | Delivers | Status |
|---|---|---|
| F0 | Skeleton, CI, real Postgres in tests | to do |
| F1 | Money and FX | to do |
| F2 | Ledger with invariants under concurrency | to do |
| F3 | Payments, state machine, base API | to do |
| F4 | Idempotency | to do |
| F5 | Simulated PIX, inbox, confirmation | to do |
| F6 | Outbox and webhooks | to do |
| F7 | Refunds and settlement | to do |
| F8 | Card, boleto, chargebacks | to do |
| F9 | Hostile simulator, CNAB reconciliation, Efí sandbox | to do |
| F10 | Public production deploy | to do |

## License

MIT. See [LICENSE](LICENSE).
