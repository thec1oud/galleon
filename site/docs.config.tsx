import { defineDocs } from "@farming-labs/docs";
import { fumadocs } from "@farming-labs/theme/default";

export default defineDocs({
  entry: "docs",
  theme: fumadocs(),
  // No github config: its "Edit on GitHub" link points at the page's own
  // file, and decision pages are generated from docs/adr, so theirs would
  // 404. Each decision page links its source record instead.
  nav: {
    title: "Galleon",
    url: "/docs",
  },
  ordering: "numeric",
  breadcrumb: { enabled: true },
  // The date would be the file's mtime, which is the checkout or sync time,
  // not when the page last changed.
  lastUpdated: false,
  pageActions: {
    alignment: "right",
    copyMarkdown: { enabled: true },
    openDocs: {
      enabled: true,
      target: "markdown",
      providers: ["chatgpt", "claude", "cursor"],
    },
  },
  metadata: {
    titleTemplate: "%s · Galleon",
    description: "Galleon is a complete, efficient web framework for Go, for internal services and large full-stack applications.",
  },
  themeToggle: { enabled: true },
  // Don't generate a docs-review workflow: it runs a third-party reusable
  // workflow from a moving branch with write permissions. CI checks the
  // docs itself.
  review: false,
  // Farming Labs sends usage events from production builds unless this is
  // off. The docs collect nothing about their readers.
  telemetry: false,
});
