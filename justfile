# The package's own binaries come first on PATH, as npm run puts them.
export PATH := justfile_directory() + "/node_modules/.bin:" + env_var("PATH")

# The npm script build.
build:
	tsc -p tsconfig.json

# The npm script check.
check:
	tsc -p tsconfig.json --noEmit

# The npm script check:harness.
check-harness:
	tsc -p tsconfig.harness.json

# The npm script test:image.
test-image:
	node --test docker/imagecheck.test.ts

# The npm script test:dashboard.
test-dashboard:
	node --test internal/api/testdata/dashboardcheck.test.ts
