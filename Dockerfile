# syntax=docker/dockerfile:1
# exchanged: the exchange API, the rail API and the market-close scheduler,
# in one static binary. Migrations run at boot under an advisory lock.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY apps/api/go.mod apps/api/go.sum ./
RUN go mod download
COPY apps/api/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/exchanged ./cmd/exchanged

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/exchanged /exchanged
ENV PORT=8081 MARKET_CLOSE_AT=12:00 MM_QUOTE_AT=10:05
EXPOSE 8081
ENTRYPOINT ["/exchanged"]
