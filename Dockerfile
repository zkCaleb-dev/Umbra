# Build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The /view page's WASM crypto module must exist before the server build
# embeds internal/api/viewassets.
RUN GOOS=js GOARCH=wasm go build -trimpath -o internal/api/viewassets/umbra.wasm ./cmd/view-wasm && \
    cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" internal/api/viewassets/ && \
    CGO_ENABLED=0 go build -trimpath -o /out/umbra ./cmd/umbra

# Run — SDF's official stellar-core image (ubuntu + core + curl), not
# distroless: the archive-backfill leg spawns captive stellar-core to
# replay history below every RPC's retention window, and the generated
# captive config fetches history-archive files with curl. Installing
# core from apt directly needs the llvm-20 runtime repo; the official
# image ships it all coherently.
FROM stellar/stellar-core:latest
RUN useradd --system --uid 10001 --create-home umbra
COPY --from=build /out/umbra /usr/local/bin/umbra
COPY deployments /deployments
ENV UMBRA_DEPLOYMENTS=/deployments/testnet.json
ENV UMBRA_CORE_BINARY=/usr/bin/stellar-core
USER umbra
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/umbra"]
