# Dev-only image for the `app` service in docker-compose.yml: Go toolchain + air.
# The source tree is bind-mounted at /src, so nothing is copied in here.
FROM golang:1.27-alpine

RUN go install github.com/air-verse/air@latest

WORKDIR /src
CMD ["air"]
