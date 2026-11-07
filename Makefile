GO_IMAGE   ?= golang:1.24
NODE_IMAGE ?= node:22-alpine

.PHONY: up down logs test test-backend test-web test-integration tidy stats

up:            ## build and start everything (UI :8080, API :8081, mock provider :9090)
	docker compose up --build

down:          ## stop and remove containers (keeps the database volume)
	docker compose down

logs:
	docker compose logs -f api

# Unit tests only: no API key, database or network service needed.
test: test-backend test-web

test-backend:  ## gofmt + go vet + go test -race (runs in Docker; no local Go needed)
	docker run --rm -v "$(CURDIR)/backend":/src -v composer-gomod:/go/pkg/mod -w /src $(GO_IMAGE) \
		sh -c 'go mod tidy && test -z "$$(gofmt -l .)" && go vet ./... && go test -race -count=1 ./...'

test-web:      ## vitest (runs in Docker on a copy, so node_modules never touches your disk)
	docker run --rm -v "$(CURDIR)/web":/app:ro -w /tmp $(NODE_IMAGE) \
		sh -c 'mkdir w && cp -r /app/. w/ && cd w && rm -rf node_modules dist && npm ci --no-audit --no-fund && npx vitest run'

test-integration: ## Postgres-backed tests (queue claim, idempotent approve, consent filter)
	docker compose --profile test run --rm backend-test

tidy:          ## generate/refresh backend/go.sum (commit it)
	docker run --rm -v "$(CURDIR)/backend":/src -v composer-gomod:/go/pkg/mod -w /src $(GO_IMAGE) go mod tidy

stats:         ## what the mock provider really delivered (max_deliveries_for_one_key must be 1)
	@curl -s localhost:9090/stats | python3 -m json.tool
