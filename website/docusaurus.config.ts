import {themes as prismThemes} from 'prism-react-renderer';
import type {Config} from '@docusaurus/types';
import type * as Preset from '@docusaurus/preset-classic';

const config: Config = {
  title: 'mindD',
  tagline: 'A framework-agnostic memory sidecar for agentic systems',
  favicon: 'img/favicon.svg',

  url: 'https://mindd.dev',
  baseUrl: '/',

  organizationName: 'vibed-project',
  projectName: 'mindD',

  onBrokenLinks: 'warn',
  markdown: {
    hooks: {
      onBrokenMarkdownLinks: 'warn',
    },
  },

  i18n: {
    defaultLocale: 'en',
    locales: ['en'],
  },

  presets: [
    [
      'classic',
      {
        docs: {
          sidebarPath: './sidebars.ts',
          routeBasePath: '/',
          editUrl:
            'https://github.com/vibed-project/mindD/tree/main/website/',
        },
        blog: false,
        theme: {
          customCss: './src/css/custom.css',
        },
      } satisfies Preset.Options,
    ],
  ],

  themeConfig: {
    image: 'img/social-card.png',
    colorMode: {
      defaultMode: 'light',
      respectPrefersColorScheme: true,
    },
    navbar: {
      title: 'mindD',
      logo: {
        alt: 'mindD logo',
        src: 'img/favicon.svg',
      },
      items: [
        {
          type: 'docSidebar',
          sidebarId: 'main',
          position: 'left',
          label: 'Docs',
        },
        {
          href: 'https://github.com/vibed-project/mindD',
          label: 'GitHub',
          position: 'right',
        },
      ],
    },
    footer: {
      style: 'dark',
      links: [
        {
          title: 'Docs',
          items: [
            {label: 'Overview', to: '/'},
            {label: 'Quickstart', to: '/quickstart'},
            {label: 'Architecture', to: '/concepts/architecture'},
          ],
        },
        {
          title: 'Building blocks',
          items: [
            {label: 'KV', to: '/blocks/kv'},
            {label: 'Episodic', to: '/blocks/episodic'},
            {label: 'Semantic', to: '/blocks/semantic'},
            {label: 'Artifact', to: '/blocks/artifact'},
            {label: 'Lease', to: '/blocks/lease'},
          ],
        },
        {
          title: 'More',
          items: [
            {label: 'GitHub', href: 'https://github.com/vibed-project/mindD'},
          ],
        },
      ],
      copyright: `Apache 2.0 · mindD contributors`,
    },
    prism: {
      theme: prismThemes.github,
      darkTheme: prismThemes.dracula,
      additionalLanguages: ['bash', 'yaml', 'protobuf', 'go', 'python', 'json', 'toml'],
    },
  } satisfies Preset.ThemeConfig,
};

export default config;
