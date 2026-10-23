# Cross-border Pay — gateway de pagamentos cross-border em Go

[![Licença: MIT](https://img.shields.io/badge/licen%C3%A7a-MIT-blue)](LICENSE)

🇺🇸 [Read in English](README.md)

Um lojista estrangeiro cobra em dólar; o pagador no Brasil paga em reais por PIX, boleto ou cartão; o sistema converte, registra tudo num ledger de partidas dobradas e repassa ao lojista em dólar. O dinheiro registrado bate com o dinheiro que existe, mesmo com falhas, duplicidade e eventos fora de ordem.

> **Projeto pessoal de aprendizado, não um gateway real.** Dinheiro simulado, sem clientes, provedores simulados. Existe para praticar a engenharia de sistemas financeiros corretos.

## Status

Escopo e decisões fechados; código a partir da F0. Plano completo em [docs/escopo.md](docs/escopo.md).

| Fase | Entrega | Status |
|---|---|---|
| F0 | Esqueleto, CI, Postgres nos testes | a fazer |
| F1 | Dinheiro e câmbio | a fazer |
| F2 | Ledger com invariantes sob concorrência | a fazer |
| F3 | Cobrança, máquina de estados, API base | a fazer |
| F4 | Idempotência | a fazer |
| F5 | PIX simulado, inbox, confirmação | a fazer |
| F6 | Outbox e webhooks | a fazer |
| F7 | Estornos e liquidação | a fazer |
| F8 | Cartão, boleto, chargebacks | a fazer |
| F9 | Simulador hostil, conciliação CNAB, sandbox Efí | a fazer |
| F10 | Produção pública | a fazer |

## Licença

MIT. Veja [LICENSE](LICENSE).
