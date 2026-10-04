import { defineConfig } from 'vitepress'

const base = '/nsc-msghub/'

export default defineConfig({
  title: 'nsc-msghub',
  description: 'Notification gateway microservice',
  base,
  cleanUrls: true,
  lastUpdated: true,
  themeConfig: {
    nav: [
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'API Reference', link: '/api/overview' },
      {
        text: 'GitHub',
        link: 'https://github.com/crazy4chicken/nsc-msghub'
      }
    ],
    sidebar: {
      '/guide/': [
        {
          text: 'Usage',
          items: [
            { text: 'Getting Started', link: '/guide/getting-started' },
            { text: 'Configuration', link: '/guide/configuration' },
            { text: 'Deployment', link: '/guide/deploy' }
          ]
        }
      ],
      '/api/': [
        {
          text: 'API Reference',
          items: [
            { text: 'Overview', link: '/api/overview' },
            {
              text: 'Reference',
              items: [
                { text: 'Channels', link: '/api/reference/channels' },
                { text: 'Notifications', link: '/api/reference/notifications' },
                { text: 'Health', link: '/api/reference/health' }
              ]
            },
            { text: 'Download OpenAPI 3.1 specification', link: `${base}openapi.yaml` }
          ]
        }
      ]
    },
    search: {
      provider: 'local'
    }
  }
})
