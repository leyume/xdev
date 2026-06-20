// Vite dev-server config tuned for running behind xdev's Caddy proxy.
export default {
  server: {
    host: true,        // listen on 0.0.0.0 inside the container
    port: 80,
    strictPort: true,
    allowedHosts: true, // accept the project's .test hostname via the proxy
  },
};
