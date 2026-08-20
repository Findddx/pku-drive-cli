SHELL := /bin/sh

ifeq ($(origin VERSION),undefined)
override VERSION := 0.3.0
else
override VERSION := $(value VERSION)
endif

ifeq ($(origin COMMIT),undefined)
override COMMIT := $(shell git rev-parse --short=12 HEAD)
else
override COMMIT := $(value COMMIT)
endif

ifeq ($(origin SOURCE_DATE_EPOCH),undefined)
override SOURCE_DATE_EPOCH := $(shell git log -1 --format=%ct)
else
override SOURCE_DATE_EPOCH := $(value SOURCE_DATE_EPOCH)
endif

ifeq ($(origin BUILD_DATE),undefined)
override BUILD_DATE :=
else
override BUILD_DATE := $(value BUILD_DATE)
endif

export VERSION
export COMMIT
export SOURCE_DATE_EPOCH
export BUILD_DATE

ifneq ($(origin PACKAGE_VERSION),undefined)
override PACKAGE_VERSION := $(value PACKAGE_VERSION)
export PACKAGE_VERSION
endif

ifneq ($(origin PACKAGE_PIN_FILE),undefined)
override PACKAGE_PIN_FILE := $(value PACKAGE_PIN_FILE)
export PACKAGE_PIN_FILE
endif

ifneq ($(origin PACKAGE_BINARY),undefined)
override PACKAGE_BINARY := $(value PACKAGE_BINARY)
export PACKAGE_BINARY
endif

ifneq ($(origin DIST_DIR),undefined)
override DIST_DIR := $(value DIST_DIR)
export DIST_DIR
endif

.NOTPARALLEL:
.PHONY: test test-build-contract build package install install-home-check

test: test-build-contract
	go test -race ./...
	go vet ./...

test-build-contract:
	@set -eu; \
	pku_contract_dir=$$(mktemp -d -p "$${PWD}" .pku-drive-contract.XXXXXX); \
	pku_marker="$$pku_contract_dir/evaluated"; \
	pku_goenv="$$pku_contract_dir/goenv"; \
	pku_gowork="$$pku_contract_dir/go.work"; \
	trap 'rm -f -- "$$pku_marker" "$$pku_goenv" "$$pku_gowork"; rmdir -- "$$pku_contract_dir" 2>/dev/null || true' 0 1 2 15; \
	for pku_metadata_name in VERSION COMMIT SOURCE_DATE_EPOCH BUILD_DATE; do \
		pku_argument="$${pku_metadata_name}=\$$(shell touch $${pku_marker})"; \
		if $(MAKE) --no-print-directory build "$$pku_argument" >/dev/null 2>&1; then echo "make function metadata was accepted: $$pku_metadata_name" >&2; exit 1; fi; \
		if [ -e "$$pku_marker" ]; then echo "make function metadata was evaluated: $$pku_metadata_name" >&2; exit 1; fi; \
	done; \
	for pku_bad_version in '$${HOME}' '"quoted"' 'has whitespace'; do \
		if $(MAKE) --no-print-directory build "VERSION=$$pku_bad_version" >/dev/null 2>&1; then echo 'unsafe literal VERSION accepted' >&2; exit 1; fi; \
	done; \
	for pku_bad_home in '//' '/..' '/tmp/..' '' 'relative'; do \
		if HOME="$$pku_bad_home" $(MAKE) --no-print-directory install-home-check >/dev/null 2>&1; then echo "unsafe HOME accepted: $$pku_bad_home" >&2; exit 1; fi; \
	done; \
	HOME="$$pku_contract_dir" $(MAKE) --no-print-directory install-home-check >/dev/null; \
	GOENV="$$pku_goenv" GOFLAGS= GOWORK=off GOTOOLCHAIN=local go env -w GOFLAGS=-race; \
	touch -- "$$pku_gowork"; \
	GOFLAGS=-race $(MAKE) --no-print-directory build >/dev/null; \
	env -u GOFLAGS GOENV="$$pku_goenv" $(MAKE) --no-print-directory build >/dev/null; \
	GOENV=off GOWORK="$$pku_gowork" $(MAKE) --no-print-directory build >/dev/null; \
	GOTOOLCHAIN=go1.99.0 $(MAKE) --no-print-directory build >/dev/null; \
	pku_expected_date=$$(date -u -d "@$${SOURCE_DATE_EPOCH}" '+%Y-%m-%dT%H:%M:%SZ'); \
	pku_expected_date=$${BUILD_DATE:-$$pku_expected_date}; \
	test "$$(./bin/pku-drive version)" = "pku-drive $${VERSION} ($${COMMIT}) $$pku_expected_date"; \
	GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local go version -m bin/pku-drive | awk '\
		$$1 == "path" && $$2 == "github.com/Findddx/pku-drive-cli/cmd/pku-drive" { module=1 } \
		$$1 == "build" && $$2 == "-trimpath=true" { trimpath=1 } \
		$$1 == "build" && $$2 == "CGO_ENABLED=0" { cgo=1 } \
		$$1 == "build" && $$2 == "GOARCH=amd64" { arch=1 } \
		$$1 == "build" && $$2 == "GOOS=linux" { os=1 } \
		$$1 == "build" && $$2 == "GOAMD64=v1" { amd64=1 } \
		END { exit !(module && trimpath && cgo && arch && os && amd64) }'

build:
	@set -eu; \
	case "$${VERSION}" in ''|*[!0-9A-Za-z._+-]*) echo 'invalid VERSION' >&2; exit 1 ;; esac; \
	case "$${COMMIT}" in ''|*[!0-9a-f]*) echo 'invalid COMMIT' >&2; exit 1 ;; esac; \
	pku_commit_length=$${#COMMIT}; \
	if [ "$$pku_commit_length" -lt 7 ] || [ "$$pku_commit_length" -gt 64 ]; then echo 'invalid COMMIT' >&2; exit 1; fi; \
	case "$${SOURCE_DATE_EPOCH}" in ''|*[!0-9]*) echo 'invalid SOURCE_DATE_EPOCH' >&2; exit 1 ;; esac; \
	pku_expected_date=$$(date -u -d "@$${SOURCE_DATE_EPOCH}" '+%Y-%m-%dT%H:%M:%SZ') || { echo 'invalid SOURCE_DATE_EPOCH' >&2; exit 1; }; \
	pku_build_date=$${BUILD_DATE:-$$pku_expected_date}; \
	case "$$pku_build_date" in ????-??-??T??:??:??Z) ;; *) echo 'invalid BUILD_DATE' >&2; exit 1 ;; esac; \
	if [ "$$pku_build_date" != "$$pku_expected_date" ]; then echo 'BUILD_DATE must match SOURCE_DATE_EPOCH' >&2; exit 1; fi; \
	pku_ldflags="-s -w -buildid= -X main.version=$${VERSION} -X main.commit=$${COMMIT} -X main.buildDate=$$pku_build_date"; \
	pku_build_dir=$$(mktemp -d -p . .pku-drive-build.XXXXXX); \
	trap 'rm -f -- "$$pku_build_dir/pku-drive"; rmdir -- "$$pku_build_dir" 2>/dev/null || true' 0 1 2 15; \
	GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local GOOS=linux GOARCH=amd64 GOAMD64=v1 CGO_ENABLED=0 go build -trimpath -buildvcs=false -ldflags "$$pku_ldflags" -o "$$pku_build_dir/pku-drive" ./cmd/pku-drive; \
	"$$pku_build_dir/pku-drive" version >/dev/null; \
	if [ -e bin ] || [ -L bin ]; then \
		if [ ! -d bin ] || [ -L bin ]; then echo 'refusing build: bin is not a project directory' >&2; exit 1; fi; \
	else \
		mkdir -- bin; \
	fi; \
	if [ -L bin/pku-drive ] || [ -d bin/pku-drive ]; then echo 'refusing build: bin/pku-drive has an unsafe type' >&2; exit 1; fi; \
	mv -fT -- "$$pku_build_dir/pku-drive" bin/pku-drive

package: build
	@./scripts/package.sh

install-home-check:
	@set -eu; \
	case $${HOME-} in /*) ;; *) echo 'refusing install: HOME must be a canonical absolute path' >&2; exit 1 ;; esac; \
	pku_canonical_home=$$(realpath -ms -- "$${HOME}"); \
	if [ "$$pku_canonical_home" = / ] || [ "$$pku_canonical_home" != "$${HOME}" ]; then \
		echo 'refusing install: HOME must be a canonical non-root absolute path' >&2; exit 1; \
	fi

install: install-home-check build
	@set -eu; \
	case $${HOME-} in /*) ;; *) echo 'refusing install: HOME must be a canonical absolute path' >&2; exit 1 ;; esac; \
	pku_canonical_home=$$(realpath -ms -- "$${HOME}"); \
	if [ "$$pku_canonical_home" = / ] || [ "$$pku_canonical_home" != "$${HOME}" ]; then \
		echo 'refusing install: HOME must be a canonical non-root absolute path' >&2; exit 1; \
	fi; \
	pku_dest_dir="$$pku_canonical_home/.local/bin"; \
	pku_dest="$$pku_dest_dir/pku-drive"; \
	install -d -m 0755 "$$pku_dest_dir"; \
	if [ -e "$$pku_dest" ] || [ -L "$$pku_dest" ]; then \
		if [ ! -f "$$pku_dest" ] || [ -L "$$pku_dest" ] || ! GOENV=off GOFLAGS= GOWORK=off GOTOOLCHAIN=local go version -m "$$pku_dest" 2>/dev/null | awk '$$1 == "path" && ($$2 == "github.com/Findddx/pku-drive-cli/cmd/pku-drive" || $$2 == "pku-drive-cli/cmd/pku-drive") { found=1 } END { exit !found }'; then \
			echo "refusing to overwrite non-project binary: $$pku_dest" >&2; exit 1; \
		fi; \
	fi; \
	pku_install_tmp=$$(mktemp "$$pku_dest_dir/.pku-drive.install.XXXXXX"); \
	trap 'rm -f -- "$$pku_install_tmp"' 0 1 2 15; \
	install -m 0755 bin/pku-drive "$$pku_install_tmp"; \
	"$$pku_install_tmp" version >/dev/null; \
	mv -fT -- "$$pku_install_tmp" "$$pku_dest"
