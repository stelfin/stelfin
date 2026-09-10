import type { MetadataRoute } from "next";

const BASE = "https://stelfin.vercel.app";

// One route, so this is short by nature. It exists because a site with no
// sitemap and no robots.txt is a site that has not told a crawler anything,
// and both are three lines.
export default function sitemap(): MetadataRoute.Sitemap {
  return [
    {
      url: BASE,
      lastModified: new Date(),
      changeFrequency: "weekly",
      priority: 1,
    },
  ];
}
