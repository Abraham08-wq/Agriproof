.PHONY: sample backup restore backup-schedule backup-off monitor-test telegram-setup payments-check setup up down logs ps test test-contracts test-backend test-ai deploy-contracts check reset open admin code deploy-testnet prod-up share

setup:            ## one-time setup: checks tools, creates .env, starts stack, deploys contract
	./scripts/setup.sh

up:               ## start everything (redeploys the contract only if it is missing)
	docker compose up -d --build postgres anvil ai
	./scripts/deploy.sh
	docker compose up -d --build api monitor

down:             ## stop everything (data and blockchain state are kept)
	docker compose down

reset:            ## stop and DELETE all AgriProof data, then run: make setup
	docker compose down -v
	sed -i 's|^CHAIN_CONTRACT_ADDRESS=.*|CHAIN_CONTRACT_ADDRESS=|' .env

logs:             ## follow API logs (Ctrl+C to stop watching)
	docker compose logs -f api

ps:
	docker compose ps

open:             ## open the console in your browser
	xdg-open http://127.0.0.1:48080 >/dev/null 2>&1 || echo "Open http://127.0.0.1:48080 in your browser"

share:            ## free public link so staff and farmers can use the field app on their phones (Ctrl+C to stop)
	./scripts/share.sh

admin:            ## create your personal admin sign-in (PROD=1 on a production server)
	./scripts/create-admin.sh

code:             ## show the latest SMS login codes while SMS is in test mode
	@docker compose logs api 2>/dev/null | grep 'TEST SMS' | grep 'code is' | tail -5 | sed -E 's/.*"to":"([^"]+)".*code is ([0-9]{6}).*/\1  code \2/' || true

deploy-testnet:   ## anchor to the public Celo Sepolia network (run twice: fund wallet, then deploy)
	./scripts/deploy-testnet.sh

prod-up:          ## start the production stack with HTTPS on a cloud server
	./scripts/prod-up.sh

deploy-contracts: ## deploy ProofLedger if missing, then restart the API with the address
	./scripts/deploy.sh
	docker compose up -d --force-recreate api

test: test-contracts test-backend test-ai

test-contracts:
	cd contracts && forge test -vv

test-backend:
	cd backend && go test ./...

test-ai:
	docker compose run --rm ai python -m pytest -q

check:            ## run the full system check (proves every protection works)
	python3 ./scripts/system-check.py

backup:           ## encrypted backup of the database, settings and local blockchain (PROD=1 on a server)
	./scripts/backup.sh

restore:          ## restore the newest backup (or FILE=path/to/file.backup); takes a safety backup first
	./scripts/restore.sh $(FILE)

backup-schedule:  ## back up automatically every day at 02:17, and show the backup passphrase to write down
	./scripts/backup-schedule.sh

backup-off:       ## turn off the daily automatic backup
	OFF=1 ./scripts/backup-schedule.sh

telegram-setup:   ## connect phone alerts: create a Telegram bot and link it (guided)
	./scripts/telegram-setup.sh

monitor-test:     ## send a test alert to your phone
	docker compose run --rm monitor python /app/monitor.py --test

payments-check:   ## check the payment company keys in .env work (reads the account balance)
	./scripts/payments-check.sh

sample:           ## set up a sample cooperative with you as manager (asks for your phone number)
	./scripts/sample.sh
