# check=skip=InvalidDefaultArgInFrom
# UI test runner: the pinned Playwright image (browsers included) + the tests of this directory.
ARG PLAYWRIGHT_IMAGE
FROM ${PLAYWRIGHT_IMAGE}
WORKDIR /ui
COPY package.json package-lock.json ./
RUN --mount=type=secret,id=extra_ca,required=false \
    if [ -s /run/secrets/extra_ca ]; then export NODE_EXTRA_CA_CERTS=/run/secrets/extra_ca; fi; \
    npm ci --no-audit --no-fund && chmod -R a+rX /ui
COPY playwright.config.ts tsconfig.json ./
COPY tests ./tests
# Runs as the invoking user (evidence files must not be root-owned).
ENV HOME=/tmp CI=1
ENTRYPOINT ["npx", "playwright", "test"]
