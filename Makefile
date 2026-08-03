.PHONY: build test vet run up down logs

build:
	go build -o bin/umbra ./cmd/umbra

test:
	go test -race ./...

vet:
	go vet ./...

run: build
	./bin/umbra

up:
	docker compose up --build -d

down:
	docker compose down

logs:
	docker compose logs -f umbra
