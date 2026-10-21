# CI image for kiln: Go, GCC 14 and Clang 18 (both can scan C++20 modules),
# the build dependencies of the corpus projects, and the pinned
# Ninja/CMake/Meson matrix from scripts/tools.sh.
#
# Build from the repository root: docker build -f ci/Dockerfile -t kiln-ci .
FROM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git make unzip xz-utils \
        python3 python3-venv python3-packaging \
        gcc g++ gcc-14 g++-14 clang-18 clang-tools-18 lld-18 \
        ccache pkg-config libffi-dev libpcre2-dev zlib1g-dev \
    && rm -rf /var/lib/apt/lists/*

ARG GO_VERSION=1.25.1
ARG GO_SHA256=7716a0d940a0f6ae8e1f3b3f4f36299dc53e31b16840dbd171254312c41ca12e
RUN curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
    && echo "${GO_SHA256}  /tmp/go.tgz" | sha256sum -c - \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH=/usr/local/go/bin:$PATH \
    GOTOOLCHAIN=local

COPY scripts/tools.sh /usr/local/bin/kiln-tools.sh
RUN kiln-tools.sh /opt/kiln-tools && rm -rf /opt/kiln-tools/dl
ENV KILN_TOOLS=/opt/kiln-tools
