import type { Metadata } from "next";
import { RootProvider } from "@farming-labs/theme";
import docsConfig from "@/docs.config";
import { channel } from "@/lib/channel";
import { ChannelBanner } from "./channel-banner";
import "./global.css";

export const metadata: Metadata = {
  title: {
    default: "Galleon",
    template: docsConfig.metadata?.titleTemplate ?? "%s",
  },
  description: docsConfig.metadata?.description,
  // Only the stable docs belong in search results.
  ...(channel().kind === "stable" ? {} : { robots: { index: false, follow: false } }),
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body>
        <ChannelBanner />
        <RootProvider>{children}</RootProvider>
      </body>
    </html>
  );
}
