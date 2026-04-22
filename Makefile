BINARY_DAEMON = bin/openqiarad
BINARY_FLASH  = bin/openqiara-flash

GOFLAGS = -trimpath
LDFLAGS = -s -w

# Camera target: ARM Linux (Sigmastar SoC)
GOOS_CAM   = linux
GOARCH_CAM = arm
GOARM_CAM  = 7

.PHONY: all build daemon flash test lint clean

all: build

build: daemon flash

daemon:
	GOOS=$(GOOS_CAM) GOARCH=$(GOARCH_CAM) GOARM=$(GOARM_CAM) \
		go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BINARY_DAEMON) ./cmd/openqiarad
	@# Compress with UPX if available — the camera's /data partition is ~20MB
	@# so a 9.6MB binary leaves little headroom for logs and upgrades.
	@if command -v upx >/dev/null 2>&1; then \
		echo "Compressing with upx..."; \
		upx --best --lzma -q $(BINARY_DAEMON) 2>&1 | tail -1; \
	else \
		echo "WARNING: upx not found — binary will be ~10MB (install: brew install upx)"; \
	fi

# flash: placeholder — not implemented yet, use scripts/sd_setup.sh
flash:
	@echo "openqiara-flash is not implemented yet. Use scripts/sd_setup.sh instead." && exit 2

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
