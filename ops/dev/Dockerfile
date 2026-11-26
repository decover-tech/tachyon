# Dev toolchain for running `make check` without installing Go, Rust or buf
# on the workstation. Versions match CI: Go from go.mod, Rust from
# hostd/rust-toolchain.toml, buf pinned below. Linux, so hostd's
# Linux-only code (setns, cgroups, vsock) compiles and its tests run.
#
#   ops/dev/run.sh make check
FROM golang:1.26.5-bookworm

ARG RUST_TOOLCHAIN=1.97.1
ARG BUF_VERSION=1.75.0

RUN apt-get update \
 && apt-get install -y --no-install-recommends build-essential pkg-config shellcheck \
 && rm -rf /var/lib/apt/lists/*

ENV RUSTUP_HOME=/usr/local/rustup CARGO_HOME=/usr/local/cargo PATH=/usr/local/cargo/bin:$PATH
RUN curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain ${RUST_TOOLCHAIN} -c clippy -c rustfmt

RUN arch="$(uname -m)" \
 && curl -sSfL -o /usr/local/bin/buf "https://github.com/bufbuild/buf/releases/download/v${BUF_VERSION}/buf-Linux-${arch}" \
 && chmod +x /usr/local/bin/buf

WORKDIR /src
