FROM golang:1.22-alpine AS build

ENV CGO_ENABLED=0
ENV GOOS=linux

WORKDIR /src

COPY go.mod go.sum ./
COPY migrations/ ./migrations/
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM alpine:3.20

RUN adduser -D -u 10001 api

COPY --from=build /out/api /usr/local/bin/api

USER api

EXPOSE 8080

CMD ["api"]
