FROM golang:alpine AS builder

ENV USER=smokescreen
ENV UID=57063
RUN adduser \
    --disabled-password \
    --gecos "" \
    --home "/nonexistent" \
    --shell "/sbin/nologin" \
    --no-create-home \
    --uid "${UID}" \
    "${USER}"

RUN apk update && apk add --no-cache git ca-certificates tzdata && mkdir /src

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-w -s" -o /bin/smokescreen

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /etc/group /etc/group
COPY --from=builder /bin/smokescreen /bin/smokescreen

USER smokescreen:smokescreen
EXPOSE 4750
ENTRYPOINT ["/bin/smokescreen"]
