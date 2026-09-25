FROM python:3.13-alpine AS fixture
WORKDIR /build
COPY fixture/Chinook_Sqlite.sqlite fixture/Chinook_Sqlite.sqlite
COPY prepare_fixture.py .
RUN python prepare_fixture.py fixture/Chinook_Sqlite.sqlite /build/output

FROM golang:1.27-alpine AS build
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY main.go main_test.go ./
COPY --from=fixture /build/output /srv/fixture
RUN CHINOOK_MANIFEST=/srv/fixture/chinook.yaml go test ./... -run '^TestPublicChinookJourney$' -count=1
RUN CGO_ENABLED=0 go build -trimpath -o /build/ovdb-cloud .

FROM scratch
COPY --from=build /build/ovdb-cloud /srv/ovdb-cloud
COPY --from=fixture /build/output /srv/fixture
COPY fixture/UPSTREAM-LICENSE.md /srv/UPSTREAM-LICENSE.md
USER 65532:65532
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/srv/ovdb-cloud"]
