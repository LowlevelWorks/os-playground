import type { NextConfig } from "next";

const nextConfig: NextConfig = {
  // Emit a fully-static site to ./out so it can be served from S3+CloudFront.
  output: "export",
  // Normalize URLs so S3/CloudFront serves /path/index.html for /path/ requests.
  trailingSlash: true,
  images: { unoptimized: true },
  turbopack: {
    rules: {
      "*.css": {
        loaders: ["@tailwindcss/turbopack"],
        as: "*.css",
      },
    },
  },
};

export default nextConfig;
