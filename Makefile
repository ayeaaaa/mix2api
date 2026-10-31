.PHONY: test build up down logs

test:
	cd app && python3 tests/run_all.py

build:
	docker compose -f deploy/docker-compose.yml build

up:
	cd deploy && docker compose up -d --build

down:
	cd deploy && docker compose down

logs:
	cd deploy && docker compose logs -f --tail=100
