# Builds a Tinode server image from THIS fork's source (server/push/apns and
# the VoipToken/Postgres changes), rather than downloading the upstream
# release tarball like docker/tinode/Dockerfile does.
#
# Everything else (bundled web client, email templates, entrypoint.sh,
# config.template) is reused unchanged from the official image - only the
# `tinode` and `init-db` binaries are replaced with ones built from this
# fork's source.
#
# Build context must be the repository root, e.g.:
#   az acr build --registry <acr-name> --image tinode-custom:latest \
#     --file docker/tinode-custom/Dockerfile .

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY . .
# go.sum may be missing entries for newly-added dependencies (e.g. sideshow/apns2) -
# `go mod tidy` resolves and downloads them here, in this remote build environment,
# so a local Go toolchain isn't required to prepare go.sum beforehand.
RUN go mod tidy
RUN go build -tags postgres -o /out/tinode ./server
RUN go build -tags postgres -o /out/init-db ./tinode-db

FROM docker.io/tinode/tinode-postgres:latest
COPY --from=build /out/tinode /opt/tinode/tinode
COPY --from=build /out/init-db /opt/tinode/init-db
# Upstream's entrypoint.sh redirects the server's stderr to a file inside the
# container, which hid every startup crash from "az containerapp logs" / Log
# Analytics. This copy only changes that one line (stderr goes to the
# container's normal stderr stream instead) - see the CUSTOMIZED comment in it.
COPY docker/tinode-custom/entrypoint.sh /opt/tinode/entrypoint.sh
RUN chmod +x /opt/tinode/entrypoint.sh
