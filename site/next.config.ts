import { withDocs } from "@farming-labs/next/config";

export default withDocs({
  devIndicators: false,
  turbopack: { root: process.cwd() },
});
