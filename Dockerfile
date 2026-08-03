# Build
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/umbra ./cmd/umbra

# Run
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/umbra /usr/local/bin/umbra
COPY deployments /deployments
ENV UMBRA_DEPLOYMENTS=/deployments/testnet.json
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/umbra"]
