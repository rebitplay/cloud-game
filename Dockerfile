ARG BUILD_PATH=/tmp/cloud-game
ARG VERSION=master

ARG GO_VERSION=1.26.4
ARG GSTREAMER_VERSION=1.29.2

# gstreamer minimal build
FROM ubuntu:resolute AS gst-builder
ARG GSTREAMER_VERSION
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get -q update && apt-get -q install --no-install-recommends -y \
    build-essential ca-certificates curl flex bison git \
    meson ninja-build pkg-config python3 \
    libglib2.0-dev libffi-dev zlib1g-dev libssl-dev \
    libopus-dev libvpx-dev liborc-0.4-dev

WORKDIR /gst

RUN git clone --single-branch --depth 1 --branch ${GSTREAMER_VERSION} \
    https://gitlab.freedesktop.org/gstreamer/gstreamer.git .

# fix meson build error
RUN sed -i '1i if get_option('"'"'doc'"'"').disabled()\n  subdir_done()\nendif' \
    subprojects/gst-plugins-good/docs/meson.build

RUN meson setup builddir \
    --prefix=/usr/local \
    --buildtype=release \
    -Doptimization=3 \
    -Db_lto=true \
    -Dauto_features=disabled \
    -Dextra-checks=disabled \
    -Dbenchmarks=disabled \
    -Dtools=disabled \
    -Dgstreamer:tools=disabled \
    -Dbase=enabled \
    -Dgood=enabled \
    -Dbad=disabled \
    -Dugly=disabled \
    -Dlibav=disabled \
    -Dges=disabled \
    -Drtsp_server=disabled \
    -Drs=disabled \
    -Dpython=disabled \
    -Dsharp=disabled \
    -Dintrospection=disabled \
    -Ddoc=disabled \
    -Dtests=disabled \
    -Dexamples=disabled \
    -Dnls=disabled \
    -Dorc=enabled \
    -Dgpl=disabled \
    -Dlibnice=disabled \
    -Dtls=disabled \
    -Dgst-plugins-base:app=enabled \
    -Dgst-plugins-base:videoconvertscale=enabled \
    -Dgst-plugins-base:audioresample=enabled \
    -Dgst-plugins-base:opus=enabled \
    -Dgst-plugins-good:vpx=enabled && \
    meson compile -C builddir && \
    meson install -C builddir && \
    find /usr/local/lib -name "*.so*" -exec strip {} \;

# base build stage
FROM ubuntu:resolute AS build0
ENV DEBIAN_FRONTEND=noninteractive
ARG GO_VERSION
ARG GO_DIST=go${GO_VERSION}.linux-amd64.tar.gz

ADD https://go.dev/dl/$GO_DIST ./
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    tar -C /usr/local -xzf $GO_DIST && rm $GO_DIST && \
    apt-get -q update && apt-get -q install --no-install-recommends -y \
    ca-certificates \
    make \
    upx-ucl && \
    rm -rf /var/lib/apt/lists/*
ENV PATH="${PATH}:/usr/local/go/bin"

# coordinator build stage
FROM build0 AS build_coordinator
ARG BUILD_PATH
ARG VERSION
ENV GIT_VERSION=${VERSION}

WORKDIR ${BUILD_PATH}
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN --mount=type=cache,target=/root/.cache/go-build make build.coordinator && \
    find ./bin/* | xargs upx -9

WORKDIR /usr/local/share/cloud-game
RUN mv ${BUILD_PATH}/bin/* ./ && \
    mv ${BUILD_PATH}/web ./web && \
    mv ${BUILD_PATH}/LICENSE ./ && \
    ${BUILD_PATH}/scripts/version.sh ./web/index.html ${VERSION} && \
    ${BUILD_PATH}/scripts/mkdirs.sh

# worker build stage
FROM build0 AS build_worker
ARG BUILD_PATH
ARG VERSION
ENV GIT_VERSION=${VERSION}

COPY --from=gst-builder /usr/local /usr/local

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get -q update && apt-get -q install --no-install-recommends -y \
    pkg-config \
    build-essential \
    libx11-dev \
    libgl1-mesa-dev \
    libglib2.0-dev && \
    rm -rf /var/lib/apt/lists/*

ENV PKG_CONFIG_PATH=/usr/local/lib/x86_64-linux-gnu/pkgconfig:/usr/local/lib/pkgconfig:/usr/local/share/pkgconfig

WORKDIR ${BUILD_PATH}
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN --mount=type=cache,target=/root/.cache/go-build make GO_TAGS=static build.worker && \
    find ./bin/* | xargs upx -9

WORKDIR /usr/local/share/cloud-game
RUN mv ${BUILD_PATH}/bin/* ./ && \
    mv ${BUILD_PATH}/LICENSE ./ && \
    ${BUILD_PATH}/scripts/mkdirs.sh worker

# coordinator runtime
FROM scratch AS coordinator

COPY --from=build_coordinator /usr/local/share/cloud-game /cloud-game
COPY --from=build_coordinator /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# worker runtime
FROM ubuntu:resolute AS worker
ENV DEBIAN_FRONTEND=noninteractive

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get -q update && apt-get -q install --no-install-recommends -y \
    curl \
    libglib2.0-0 \
    libopus0 \
    libvpx12 \
    liborc-0.4-0 \
    libx11-6 \
    libxext6 && \
    rm -rf /var/lib/apt/lists/* /var/log/* /usr/share/bug /usr/share/doc /usr/share/doc-base \
    /usr/share/X11/locale/*

COPY --from=gst-builder /usr/local/lib/x86_64-linux-gnu/libgst* /usr/local/lib/x86_64-linux-gnu/
COPY --from=gst-builder /usr/local/lib/x86_64-linux-gnu/gstreamer-1.0/ /usr/local/lib/x86_64-linux-gnu/gstreamer-1.0/

ENV LD_LIBRARY_PATH=/usr/local/lib/x86_64-linux-gnu
ENV GST_PLUGIN_PATH=/usr/local/lib/x86_64-linux-gnu/gstreamer-1.0

COPY --from=build_worker /usr/local/share/cloud-game /cloud-game
COPY --from=build_worker /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

ADD https://github.com/sergystepanov/mesa-llvmpipe/releases/download/v1.0.0/libGL.so.1.5.0 \
    /usr/lib/x86_64-linux-gnu/
RUN cd /usr/lib/x86_64-linux-gnu && \
    rm -f libGL.so.1 libGL.so && \
    ln -s libGL.so.1.5.0 libGL.so.1 && \
    ln -s libGL.so.1 libGL.so

# final image
FROM worker AS cloud-game

WORKDIR /usr/local/share/cloud-game

COPY --from=coordinator /cloud-game ./
COPY --from=worker /cloud-game ./

# Single-container test image for platforms where one app/container is simpler.
FROM cloud-game AS bunny-magic

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get -q update && apt-get -q install --no-install-recommends -y \
    xvfb && \
    rm -rf /var/lib/apt/lists/* /var/log/* /usr/share/bug /usr/share/doc /usr/share/doc-base

COPY assets/cores/melondslan_libretro.so ./assets/cores/
COPY assets/games/nds/blocksds-local-multiplayer.nds ./assets/games/nds/
COPY scripts/bunny-entrypoint.sh ./bunny-entrypoint.sh

RUN useradd --system --home /usr/local/share/cloud-game --shell /usr/sbin/nologin cloudgame && \
    mkdir -p /tmp/cloud-game && \
    chown -R cloudgame:cloudgame /usr/local/share/cloud-game /tmp/cloud-game

USER cloudgame

ENTRYPOINT ["./bunny-entrypoint.sh"]
