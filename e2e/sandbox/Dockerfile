ARG GO_VERSION

FROM golang:${GO_VERSION}-alpine AS go

FROM docker:29.9.1-dind

COPY --from=go /usr/local/go /usr/local/go

RUN apk add --no-cache \
        bash \
        bzip2 \
        composer \
        curl \
        git \
        just \
        mongodb-tools \
        php85-ctype \
        php85-dom \
        php85-fileinfo \
        php85-mbstring \
        php85-pdo \
        php85-pdo_mysql \
        php85-session \
        php85-tokenizer \
        php85-xml \
        php85-xmlwriter \
        redis \
        xz \
        zstd

ENV PATH=/root/go/bin:/usr/local/go/bin:$PATH \
    GOTOOLCHAIN=local \
    GOFLAGS=-buildvcs=false \
    TREEMAN_E2E_SANDBOX=1

COPY entrypoint.sh /usr/local/bin/treeman-e2e-sandbox

WORKDIR /src

ENTRYPOINT ["treeman-e2e-sandbox"]
