// Vite config for a static app running on the host behind xdev's Caddy proxy.
// xdev passes --host/--port on the command line (binding the port it allocated),
// so this only needs allowedHosts so Vite accepts the project's .test hostname
// that the proxy forwards.
export default {
  server: {
    allowedHosts: true,
  },
};
