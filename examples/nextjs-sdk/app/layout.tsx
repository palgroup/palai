import type { ReactNode } from "react";

export const metadata = {
  title: "Palai SDK — live response proof",
  description: "A Next.js consumer of the Palai TypeScript SDK, streaming canonical events to the browser.",
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
