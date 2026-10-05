# syntax=docker/dockerfile:1.2
FROM scratch
COPY --chown=0:0 --chmod=0555 ovdb-cloud /srv/ovdb-cloud
# Preparation rejects nested/hidden entries; generated artifacts are flat files.
COPY --chown=0:0 --chmod=0444 fixture/output/* /srv/fixture/
COPY --chown=0:0 --chmod=0444 fixture/UPSTREAM-LICENSE.md /srv/CHINOOK-UPSTREAM-LICENSE.md
# The child tmp directory gets its mode from COPY, including in a scratch image.
COPY --chown=0:0 --chmod=1777 image-layout/ /
USER 65532:65532
ENV PORT=8080 OVDB_SELECTED_STORAGE=protected-image
EXPOSE 8080
ENTRYPOINT ["/srv/ovdb-cloud"]
