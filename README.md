# AgriProof

Honest weights, genuine fertilizer and fair pay for farming cooperatives.

- **Every delivery is weighed into a sealed record.** The farmer gets an SMS receipt at once. Nobody can quietly change a weight later: the records are chained by fingerprint and sealed on a blockchain.
- **Fake fertilizer is caught before it reaches the field.** Every genuine bag carries its own code. A farmer checks it by SMS or on the check page before buying. Fake codes, copied labels and recalled batches are refused.
- **Farmers are paid by M-Pesa.** One manager prepares the payments and a second one approves them. Each payment is written into the sealed records.
- **AI watches for fraud.** It holds ghost farmers, deliveries bigger than the farm can grow, and weights from a clerk that stand out. It also compares harvests from farms that used each fertilizer batch. A person always makes the final decision.

AgriProof works with any crop. Maize, beans, coffee, tea, wheat, sorghum, potatoes and avocado are ready to use.

It is a separate system from ChamaChain. It uses its own database and its own ports (48xxx), and it shares no data with ChamaChain. Both can run on the same computer.

## What's inside

| Folder | What it is |
| --- | --- |
| `backend/` | Go API. It also serves the four pages: the **field office** console, the **field app** for staff and farmers, the **bag check** page and the **buyer proof** page |
| `ai-service/` | Python fraud checks: ghost farmers, delivery checks, code-check patterns, harvest comparison per batch |
| `contracts/` | `ProofLedger`, the blockchain contract that seals the record fingerprints |
| `monitor/` | The watchman: sends Telegram alerts to your phone |
| `scripts/` | Setup, admin account, sample cooperative, system check, backups, going online |
| `brand/` | Farm photos (downloaded by `make setup`) |

## Install or update (Ubuntu)

First time only:

```bash
sudo apt update && sudo apt install -y git curl jq make openssl unzip qrencode python3
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER && newgrp docker
```

Then, each time you get a new zip:

```bash
cd ~ && unzip -o agriproof.zip
cd ~/agriproof
chmod +x scripts/*.sh scripts/*.py
make setup
```

`make setup` keeps your existing `.env` and data. On a first install, also run:

```bash
make admin     # your personal sign-in: username, password and an authenticator-app QR code
make sample    # optional: a sample cooperative with you as manager, prices and 20 fertilizer bag codes
```

Open the field office at **http://127.0.0.1:48080** and sign in.

## The four pages

| Address | Who uses it |
| --- | --- |
| `http://127.0.0.1:48080/` | **Field office** (you): cooperatives, AI flags, suppliers and batches, money, activity log |
| `/app/` | **Field app** for cooperative staff and farmers. Staff weigh deliveries, register farmers, hand out fertilizer and pay farmers. Farmers see their deliveries, payments and inputs |
| `/check` | **Bag check.** Anyone can use it, no sign-in needed. Type the code on a fertilizer or seed bag |
| `/p/CODE` | **Buyer proof.** Shows where a lot of produce came from, that its records are intact and how much of the farmers' money is paid. It shows no personal details |

To put the field app on staff and farmers' phones for free:

```bash
make share
```

This prints a public link and a QR code. Send the link by WhatsApp. People sign in with their phone number and an SMS code, then tap **Install app**. While SMS is in test mode, run `make code` in a second terminal to see the login codes.

## How the fertilizer check works

1. In the field office, **add the supplier** and **verify** them only after you have checked their company registration and KEBS permit yourself.
2. **Register the batch**, exactly as printed on the bags.
3. **Create and download the codes.** You get one unique code per bag, in a spreadsheet for the label printer. They are shown only this once. AgriProof keeps only their fingerprints, so nobody can copy them from the system.
4. **Ship** bags to a cooperative, and record the **lab result** when it comes back.
5. Farmers **check** a bag before buying. A code that does not exist is called a fake. A code already checked from another phone is a copied label. If a batch is recalled, everyone who received or checked it gets an SMS.
6. **Compare harvests.** For any batch, the AI compares harvests from farms that used it with farms that did not.

**Checking by SMS:** in Africa's Talking, set the incoming-message callback to

```
https://YOUR-ADDRESS/sms/inbound/<INBOUND_SECRET from .env>
```

A farmer then texts the bag code to your number and gets the answer back by SMS.

## Money

- **Test mode** (the default): no real money moves. When a manager adds money, you confirm it by hand under **Money**, but only once the money is really in.
- **Live:** add your IntaSend keys (or Safaricom Daraja keys) to `.env`, run `make payments-check`, then `make up`. From then on, money counts only when the payment company confirms it.
- A pay run is prepared by one manager and approved by a different manager (or by you), and only if the cooperative has enough money to cover the farmers and the payment fees (`PAYOUT_FEE_RESERVE` per farmer is held back until the real fee is known). No farmer can get more than `PAYOUT_MAX` in one payment. When the payment company's answer is unclear, the payment stops and waits for a person. Before it can be sent again, AgriProof asks the payment company whether it went through, or asks you to write down what you checked, so no farmer is paid twice.
- A daily money check compares the payment account with the records. If they differ while live, payments pause and you get an alert. They stay paused until a check comes back balanced.

## Everyday commands

| Command | What it does |
| --- | --- |
| `make up` / `make down` | Start / stop (your data is kept) |
| `make check` | Full system check. It runs only in test mode and proves every protection works |
| `make backup` / `make restore` | Encrypted backup / restore. `make backup-schedule` backs up every day |
| `make telegram-setup` | Sends alerts to your phone: AI flags, fake-code bursts, money problems, system down |
| `make logs` | Watch what the system is doing |
| `make deploy-testnet` | Seal records on the public Celo Sepolia blockchain instead of the local one |
| `make prod-up` | Run on a cloud server with HTTPS (set `DOMAIN` in `.env` first) |

Ports: field office and app 48080, database 48432, local blockchain 48545, AI 48800. They are reachable only from this computer.

## Photos

The farm photos are free under the [Unsplash License](https://unsplash.com/license). They are by Eliézer Fernandes, Noah Buscher, Bernd Dittrich and Roger Starnes Sr. `make setup` saves them into `brand/`, so the pages work without internet.
