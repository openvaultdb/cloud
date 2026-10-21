# syntax=docker/dockerfile:1.2
FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS trust
FROM scratch
COPY --from=trust --chown=0:0 --chmod=0444 /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --chown=0:0 --chmod=0555 ovdb-cloud /srv/ovdb-cloud
# Preparation closes the native inGitDB tree to directories 0555 and files 0444;
# Docker preserves those modes while copying the complete verified fixture tree.
COPY --chown=0:0 fixture/output/ /srv/fixture/
COPY --chown=0:0 --chmod=0444 fixture/UPSTREAM-LICENSE.md /srv/CHINOOK-UPSTREAM-LICENSE.md
# The child tmp directory gets its mode from COPY, including in a scratch image.
COPY --chown=0:0 --chmod=1777 image-layout/ /
USER 65532:65532
# dalgo2ingitdb keeps only transaction lock sidecars here; source records remain
# direct reads from the immutable native tree and no parsed data is cached.
ENV PORT=8080 OVDB_SELECTED_STORAGE=protected-image XDG_CACHE_HOME=/tmp/.cache
EXPOSE 8080
ENTRYPOINT ["/srv/ovdb-cloud"]
