import { ComparePanel } from "@/components/compare-panel";

// A Server Component shell, the same shape app/page.tsx uses: all the interaction lives in the
// ComparePanel Client Component, which talks only to the /api/palai/compare Route Handler —
// never to the control plane, never with a key.
export default function Page() {
  return <ComparePanel />;
}
