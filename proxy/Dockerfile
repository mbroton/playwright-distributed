FROM golang:1.26.5-alpine3.24 AS build

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/proxy ./cmd/proxy

FROM alpine:3.24.1

WORKDIR /app

COPY --from=build /app/proxy /app/proxy

ENTRYPOINT ["/app/proxy"]
