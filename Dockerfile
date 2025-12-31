FROM golang:1.22-alpine AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/callback-listener ./

FROM alpine:3.19

WORKDIR /app

COPY --from=build /out/callback-listener /app/callback-listener
COPY templates/ /app/templates/
COPY static/ /app/static/
COPY config.yml /app/config.yml

EXPOSE 8080

ENTRYPOINT ["/app/callback-listener"]
