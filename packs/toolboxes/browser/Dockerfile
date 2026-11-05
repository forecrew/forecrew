# The browser toolbox (browser.yaml): Playwright's MCP server and Chromium on
# top of the agent base image. Versions are exact in browser/package.json;
# package-lock.json pins every dependency with its integrity hash. Build
# context: packs/toolboxes.
ARG FORECREW_BASE
FROM ${FORECREW_BASE}

# Browsers live outside any home directory, readable by every user: jobs run
# as the worker's uid.
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/ms-playwright
COPY browser/package.json browser/package-lock.json /opt/playwright-mcp/
RUN cd /opt/playwright-mcp && npm ci --omit=dev --no-audit --no-fund \
    && ./node_modules/.bin/playwright-core install --with-deps chromium \
    && chmod -R a+rX /opt/ms-playwright /opt/playwright-mcp \
    && rm -rf /var/lib/apt/lists/* \
    && npm cache clean --force
ENV PATH=/opt/playwright-mcp/node_modules/.bin:$PATH
