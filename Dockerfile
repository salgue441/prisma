# syntax=docker/dockerfile:1
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/prisma ./cmd/prisma

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/prisma /prisma
COPY configs/prisma.yaml /etc/prisma/prisma.yaml
USER nonroot:nonroot
EXPOSE 8080 9901
ENTRYPOINT ["/prisma", "-config", "/etc/prisma/prisma.yaml"]
