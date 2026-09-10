.PHONY: build package test vet check clean

VERSION ?= dev

build:
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o easyconnect .

package:
	VERSION="$(VERSION)" ./scripts/package.sh

test:
	CGO_ENABLED=0 go test -buildvcs=false ./...

vet:
	CGO_ENABLED=0 go vet ./...

check: test vet build

clean:
	$(RM) easyconnect
	$(RM) -r dist
