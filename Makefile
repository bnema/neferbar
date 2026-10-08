.PHONY: bin install build test vet race fmt-check mod-check check pkg

# Build a fresh binary.
bin:
	CGO_ENABLED=0 go build -o bin/neferbar ./cmd/neferbar
install:
	CGO_ENABLED=0 go install ./cmd/neferbar
build:
	CGO_ENABLED=0 go build ./...
test:
	CGO_ENABLED=0 go test ./...
vet:
	CGO_ENABLED=0 go vet ./...
race:
	# cgo is enabled only for the race test binary.
	CGO_ENABLED=1 go test -race ./...
# Go sources must be gofmt-clean (lists the offending files and fails otherwise).
fmt-check:
	test -z "$$(gofmt -l ./internal ./cmd)" || { gofmt -l ./internal ./cmd >&2; exit 1; }
# go.mod and go.sum must be tidy (prints the diff and fails otherwise).
mod-check:
	go mod tidy -diff
check: fmt-check mod-check vet test

# Arch package of the committed HEAD (packaging/arch/PKGBUILD). Go modules
# come from the module proxy in prepare(); the build itself runs offline.
# pacman-ordered version: <tag>.r<commits>.g<hash>, or 0.0.0 without a tag.
pkg: SHELL := bash
pkg: .SHELLFLAGS := -eo pipefail -c
pkg:
	@test -z "$$(git status --porcelain)" || echo "warning: uncommitted changes are not packaged" >&2
	v=$$(t=$$(git describe --tags --abbrev=0 2>/dev/null | sed 's/^v//; s/-/_/g'); \
		echo "$${t:-0.0.0}.r$$(git rev-list --count HEAD).g$$(git rev-parse --short HEAD)"); \
	d=$$(mktemp -d /tmp/neferbar-pkg.XXXXXX); trap 'rm -rf "$$d"' EXIT; \
	git archive --prefix=neferbar-$$v/ -o "$$d/neferbar-$$v.tar.gz" HEAD; \
	cp packaging/arch/PKGBUILD "$$d/"; \
	cd "$$d" && sed -i "s/^pkgver=.*/pkgver=$$v/; s/^sha256sums=.*/sha256sums=('$$(sha256sum *.tar.gz | cut -d' ' -f1)')/" PKGBUILD; \
	makepkg -f --noconfirm; mkdir -p $(CURDIR)/dist; rm -f $(CURDIR)/dist/neferbar-*.pkg.tar.zst; mv *.pkg.tar.zst $(CURDIR)/dist/
	@ls dist/*.pkg.tar.zst
