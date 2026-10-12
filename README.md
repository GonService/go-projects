# Go projects

Small services and tools in Go by [GonService](https://gonserviceit.com.br/devops). Each folder is an
independent project with its own README, tests and Dockerfile; CI formats, vets, tests (with the race
detector) and builds every one of them.

| Project | What it shows | Stack |
| --- | --- | --- |
| [status-page](status-page) | Public status page with scheduled HTTP checks, JSON API and Prometheus metrics | Go standard library, distroless |
| [url-shortener](url-shortener) | Short links with custom aliases, click stats per day and referrer, JSON API, atomic JSON-file storage | Go standard library, distroless |
| [nginx-log-analyzer](nginx-log-analyzer) | CLI that streams Nginx access logs (gzip, stdin) into a table, JSON or HTML report with latency and error spikes | Go standard library, distroless |
| [ssl-expiry-checker](ssl-expiry-checker) | Concurrent TLS certificate checks with CI exit codes, HTML report and a Prometheus daemon mode | Go standard library, distroless |
| [api-gateway](api-gateway) | Reverse proxy with API keys, per-client rate limits, health-checked round robin, access logs and status page | Go standard library, distroless |

## Em português

Serviços e ferramentas em Go, pequenos e rápidos. Precisa de monitoramento, uma API ou uma ferramenta
interna? Fale com a GonService: [gonserviceit.com.br/devops](https://gonserviceit.com.br/devops) ·
[contato@gonserviceit.com.br](mailto:contato@gonserviceit.com.br)

## License

[MIT](LICENSE)
