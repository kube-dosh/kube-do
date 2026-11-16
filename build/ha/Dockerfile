# syntax=docker/dockerfile:1

ARG BASE_IMAGE=almalinux:9
FROM ${BASE_IMAGE} AS build

ARG COMPONENT
ARG VERSION
ARG SOURCE_URL
ARG SOURCE_SHA256

RUN dnf install -y --setopt=install_weak_deps=False \
      diffutils \
      gcc \
      glibc-devel \
      gzip \
      make \
      openssl-devel \
      tar \
    && dnf clean all

RUN set -eux; \
    curl --fail --location --retry 3 --output /tmp/source.tar.gz "${SOURCE_URL}"; \
    echo "${SOURCE_SHA256}  /tmp/source.tar.gz" | sha256sum --check --strict -; \
    mkdir -p /src; \
    tar -xzf /tmp/source.tar.gz -C /src --strip-components=1

WORKDIR /src
RUN set -eux; \
    case "${COMPONENT}" in \
      haproxy) \
        make -j"$(nproc)" TARGET=linux-glibc USE_OPENSSL=0 USE_PCRE=0 USE_ZLIB=0 USE_THREAD=1 USE_LIBCRYPT=0; \
        install -D -m 0755 haproxy /out/usr/local/sbin/haproxy; \
        ;; \
      keepalived) \
        ./configure --disable-snmp --disable-dbus --disable-json --disable-libnl --disable-nftables; \
        make -j"$(nproc)"; \
        install -D -m 0755 bin/keepalived /out/usr/local/sbin/keepalived; \
        ;; \
      *) \
        echo "unsupported component: ${COMPONENT}" >&2; \
        exit 1; \
        ;; \
    esac; \
    binary="/out/usr/local/sbin/${COMPONENT}"; \
    ldd "${binary}" | awk '/=> not found/ { exit 1 } /=> \// { print $3 } /^[[:space:]]*\// { print $1 }' | sort -u > /tmp/libraries; \
    while IFS= read -r library; do cp --parents "${library}" /out; done < /tmp/libraries; \
    for library in /lib64/libgcc_s.so.1 /usr/lib64/libgcc_s.so.1; do \
      if [ -f "${library}" ]; then cp --parents "${library}" /out; fi; \
    done; \
    install -D -m 0644 /dev/null /out/etc/passwd; \
    printf 'root:x:0:0:root:/root:/sbin/nologin\nnobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin\n' > /out/etc/passwd; \
    printf 'root:x:0:\nnobody:x:65534:\n' > /out/etc/group; \
    printf 'passwd: files\ngroup: files\nhosts: files\n' > /out/etc/nsswitch.conf

FROM scratch
ARG COMPONENT
ARG VERSION
COPY --from=build /out/ /
LABEL org.opencontainers.image.source="https://github.com/kube-dosh/kube-do" \
      org.opencontainers.image.version="${VERSION}" \
      io.kube-do.installer.component="${COMPONENT}"

