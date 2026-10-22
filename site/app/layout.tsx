import type { Metadata } from "next";
import { RootProvider } from "@farming-labs/theme";
import docsConfig from "@/docs.config";
import "./global.css";

export const metadata: Metadata = {
  title: {
    default: "Galleon",
    template: docsConfig.metadata?.titleTemplate ?? "%s",
  },
  description: docsConfig.metadata?.description,
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body>
        <RootProvider>{children}</RootProvider>
      </body>
    </html>
  );
}
