# Loft build targets. Go builds directly; the web app and CLI npm packaging shell out.
.PHONY: build daemon cli web test cli-dist clean

# Build the daemon and CLI binaries into bin/.
build: daemon cli

daemon:
	go build -o bin/loftd ./cmd/loftd

cli:
	go build -o bin/loft ./cmd/loft

# Build the web root site.
web:
	cd web && pnpm install --frozen-lockfile && pnpm run build

# Run the black-box acceptance suite (needs Docker for Postgres + the LLM emulator).
test:
	cd test && pnpm install && pnpm test

# Cross-compile the CLI for every platform into the npm/ packages, ready to publish loft-cli.
cli-dist:
	./npm/build.sh

clean:
	rm -rf bin npm/loft-cli-*/bin
