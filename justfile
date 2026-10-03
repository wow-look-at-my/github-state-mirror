# Compile internal/api/web/src/*.ts to the gitignored assets/*.js that the Go build embeds.
build:
	npx tsc -p tsconfig.json

# Type-check the dashboard sources without writing output.
check:
	npx tsc -p tsconfig.json --noEmit

# Type-check the browser and image test harnesses.
check-harness:
	npx tsc -p tsconfig.harness.json

# Start the built image and require it to serve. Needs docker and build/server.
test-image:
	node --test docker/imagecheck.test.ts

# Drive the built dashboard in a browser.
test-dashboard:
	node --test internal/api/testdata/dashboardcheck.test.ts
