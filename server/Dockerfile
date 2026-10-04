FROM scratch
COPY ovdb-cloud /srv/ovdb-cloud
COPY fixture/output /srv/fixture
COPY fixture/UPSTREAM-LICENSE.md /srv/CHINOOK-UPSTREAM-LICENSE.md
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/srv/ovdb-cloud"]
