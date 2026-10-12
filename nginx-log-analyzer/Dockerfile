FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/nginx-log-analyzer .

# Distroless: no shell, no package manager, runs as non-root
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nginx-log-analyzer /nginx-log-analyzer
WORKDIR /logs
ENTRYPOINT ["/nginx-log-analyzer"]
