FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ssl-expiry-checker .

# Distroless: no shell, no package manager, runs as non-root; includes CA certificates
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ssl-expiry-checker /ssl-expiry-checker
COPY targets.example.txt /targets.txt
EXPOSE 9115
ENTRYPOINT ["/ssl-expiry-checker"]
CMD ["-f", "/targets.txt"]
