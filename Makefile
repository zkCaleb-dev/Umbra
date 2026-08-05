.PHONY: build wasm test vet run up down logs

# The /view page's crypto module. Build BEFORE the server binary so the
# embedded assets carry it (a plain `go build` still works — the page
# then 404s its assets with a pointer here).
wasm:
	GOOS=js GOARCH=wasm go build -trimpath -o internal/api/viewassets/umbra.wasm ./cmd/view-wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/api/viewassets/

build: wasm
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
