# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags='-s -w' -o /out/dieyet ./cmd/dieyet

# Runtime: the binary and CA certificates (for Open Food Facts over HTTPS).
# No shell, no package manager. The database is the workout app's PostgreSQL,
# reached through DIEYET_DATABASE_URL.
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/dieyet /dieyet
USER 65532:65532
ENV DIEYET_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/dieyet"]
