FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api-gateway .

# Distroless: no shell, no package manager, runs as non-root
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api-gateway /api-gateway
COPY config.example.json /config.json
# 8080: public traffic, 9090: status page and metrics (keep it private)
EXPOSE 8080 9090
ENTRYPOINT ["/api-gateway", "-config", "/config.json"]
