.PHONY: test test-hub test-codearts build up down logs

test: test-hub test-codearts

test-hub:
	cd apps/hub && python3 tests/run_all.py

test-codearts:
	cd apps/codearts && go test ./...

build:
	docker compose -f deploy/docker-compose.yml --env-file deploy/.env.example build

up:
	cd deploy && docker compose up -d --build

down:
	cd deploy && docker compose down

logs:
	cd deploy && docker compose logs -f --tail=100
