# memsidecar docs site

[Docusaurus](https://docusaurus.io) site for memsidecar.

## Develop

```bash
cd website
npm install
npm run start          # http://localhost:3000
```

## Build

```bash
npm run build          # static HTML in ./build/
npm run serve          # serve the built site locally
```

## Layout

```
website/
├── docs/                       # source markdown
│   ├── intro.md
│   ├── quickstart.md
│   ├── concepts/
│   ├── blocks/
│   ├── config/
│   ├── ops/
│   ├── clients/
│   ├── deploy/
│   └── reference/
├── src/css/custom.css          # theme overrides
├── static/img/favicon.svg
├── sidebars.ts
├── docusaurus.config.ts
└── package.json
```

The Makefile at the repo root wires `make docs-dev`, `make docs-build`,
and `make docs-clean` as shortcuts.
