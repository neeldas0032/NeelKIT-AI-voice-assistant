/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  // The browser talks to neelkitd directly, so the API base URL is the one
  // piece of configuration the page needs at runtime. It is public by design:
  // the API key never reaches the bundle, only the browser's own storage.
  env: {
    NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL || 'http://127.0.0.1:8420',
  },
  output: 'standalone',
  // Without this, Next traces up to the repository root (it sees the Go module
  // beside web/) and nests the standalone bundle under neelkit/web/, so the
  // Dockerfile's COPY lands server.js somewhere nothing expects.
  outputFileTracingRoot: import.meta.dirname,
};

export default nextConfig;
