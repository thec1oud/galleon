import { withDocs } from "@farming-labs/next/config";
import { channel } from "./lib/channel";

export default withDocs({
  devIndicators: false,
  turbopack: { root: process.cwd() },
  // Only the stable docs belong in search results. The pages say so in
  // their metadata too, but this also covers llms.txt, the sitemap and the
  // other routes that aren't pages.
  async headers() {
    if (channel().kind === "stable") return [];
    return [{ source: "/:path*", headers: [{ key: "X-Robots-Tag", value: "noindex" }] }];
  },
});
