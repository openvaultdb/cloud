FROM python:3.13-alpine AS fixture
WORKDIR /build
COPY fixture/Chinook_Sqlite.sqlite fixture/Chinook_Sqlite.sqlite
COPY prepare_fixture.py .
RUN python prepare_fixture.py fixture/Chinook_Sqlite.sqlite /build/output

FROM scratch
COPY ovdb-cloud /srv/ovdb-cloud
COPY --from=fixture /build/output /srv/fixture
COPY fixture/UPSTREAM-LICENSE.md /srv/UPSTREAM-LICENSE.md
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/srv/ovdb-cloud"]
