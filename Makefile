.PHONY: build up down logs ps migrate test fmt lint generate

build:
	docker compose build

up: build
	docker compose up -d

down:
	docker compose down $(if $(V),-v,)

logs:
	docker compose logs -f bot worker api

ps:
	docker compose ps

migrate:
	docker compose run --rm migrate

test:
	go test ./... -race -count=1

fmt:
	gofmt -s -w . && go vet ./...

lint:
	golangci-lint run ./...

.env:
	cp .env.example .env
	@echo "Заполните .env перед запуском"
