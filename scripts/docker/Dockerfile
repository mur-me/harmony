ARG GOPATH_DEFAULT=/root/go
ARG SRC_PATH=src/github.com/harmony-one
ARG BUILD_BRANCH=main

FROM golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac AS builder

ARG GOPATH_DEFAULT
ARG SRC_PATH
ARG BUILD_BRANCH

ENV GOPATH=${GOPATH_DEFAULT}
ENV GO111MODULE=on
ENV HMY_PATH=${GOPATH}/${SRC_PATH}
ENV BRANCH=${BUILD_BRANCH}

ENV PACKAGES="libgmp-dev libssl-dev ca-certificates curl git psmisc dnsutils jq make gcc g++ bash"

# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends ${PACKAGES} \
    && rm -rf /var/lib/apt/lists/*

WORKDIR ${HMY_PATH}

RUN git clone --depth=1 --branch "${BRANCH}" https://github.com/mur-me/harmony.git harmony \
    && git clone --depth=1 --branch master https://github.com/harmony-one/bls.git bls \
    && git clone --depth=1 --branch master https://github.com/harmony-one/mcl.git mcl

WORKDIR ${HMY_PATH}/harmony

RUN make linux_static

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

ARG GOPATH_DEFAULT
ARG SRC_PATH

ARG HARMONY_USER=harmony
ARG HARMONY_USER_UID=1000
ARG HARMONY_USER_GID=1000

ENV HARMONY_HOME=/harmony
ENV HOME=${HARMONY_HOME}

# hadolint ignore=DL3018
RUN apk add --no-cache bash bind-tools tini curl sed \
    && addgroup -g "${HARMONY_USER_GID}" "${HARMONY_USER}" \
    && adduser -u "${HARMONY_USER_UID}" -G "${HARMONY_USER}" --shell /sbin/nologin --no-create-home -D "${HARMONY_USER}" \
    && addgroup "${HARMONY_USER}" tty \
    && sed -i -e "s/bin\/sh/bin\/bash/" /etc/passwd

RUN echo "[ ! -z \"\$TERM\" -a -r /etc/motd ] && cat /etc/motd" >> /etc/bash/bashrc

WORKDIR ${HARMONY_HOME}

COPY --from=builder ${GOPATH_DEFAULT}/${SRC_PATH}/harmony/bin/harmony /usr/local/bin/

RUN chmod +x /usr/local/bin/harmony \
    && mkdir -p /data \
    && chown -R "${HARMONY_USER_UID}:${HARMONY_USER_GID}" "${HARMONY_HOME}" /data

VOLUME /data

USER ${HARMONY_USER_UID}:${HARMONY_USER_GID}

EXPOSE 9000/tcp 9500/tcp 9800/tcp 6000/tcp

ENTRYPOINT ["tini", "--"]
CMD ["harmony", "-c", "harmony.conf"]
