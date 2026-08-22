import type {SidebarsConfig} from '@docusaurus/plugin-content-docs';

const sidebars: SidebarsConfig = {
  main: [
    'intro',
    'quickstart',
    'guides/use-cases',
    {
      type: 'category',
      label: 'Concepts',
      collapsed: false,
      items: [
        'concepts/architecture',
        'concepts/capabilities',
        'concepts/namespaces',
        'concepts/policy',
        'concepts/tenant-isolation',
        'concepts/encryption-at-rest',
      ],
    },
    {
      type: 'category',
      label: 'Building blocks',
      collapsed: false,
      items: [
        'blocks/kv',
        'blocks/episodic',
        'blocks/semantic',
        'blocks/artifact',
        'blocks/lease',
        'blocks/graph',
        'blocks/admin',
      ],
    },
    {
      type: 'category',
      label: 'Configuration',
      items: [
        'config/reference',
        'config/hot-reload',
      ],
    },
    {
      type: 'category',
      label: 'Operations',
      items: [
        'ops/tls',
        'ops/key-rotation',
        'ops/observability',
      ],
    },
    {
      type: 'category',
      label: 'Clients',
      items: [
        'clients/python',
        'clients/http-gateway',
        'clients/grpc',
        'clients/mcp',
      ],
    },
    {
      type: 'category',
      label: 'Deployment',
      items: [
        'deploy/docker',
        'deploy/helm',
      ],
    },
  ],
};

export default sidebars;
