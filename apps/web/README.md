# os-playground web

## Who can open a machine

The browser makes a random owner token on first visit (32 bytes, base64url,
in `localStorage` under `os-playground.token`) and sends it with every call:
`Authorization: Bearer <token>` on REST, and as the WebSocket subprotocol
`token.<token>` next to `os-playground.v1`, because a browser cannot set
headers on a WebSocket. The token never goes in a URL.

Only the browser that created a machine can see, attach to or delete it;
anyone else gets the same 404 as for a machine that does not exist. There are
no accounts: clearing site data, a private window or another browser is a new
owner. Its old machine is removed by the server once it has been idle
(`PLAYGROUND_IDLE_TIMEOUT`, 15 minutes by default).

Each network address may hold `PLAYGROUND_MAX_VMS_PER_CLIENT` machines (1 by
default). Over that, starting a machine offers "Resume it" when the machine in
the way is this browser's, and otherwise says someone on the same network is
using it. The machine's ID is kept in `localStorage` too, so a new tab can
resume it.

If storage is blocked the token lives in memory only; the page says so,
because a reload then loses the session.

## Next.js

This is a [Next.js](https://nextjs.org) project bootstrapped with [`create-next-app`](https://nextjs.org/docs/app/api-reference/cli/create-next-app).

## Getting Started

First, run the development server:

```bash
npm run dev
# or
yarn dev
# or
pnpm dev
# or
bun dev
```

Open [http://localhost:3000](http://localhost:3000) with your browser to see the result.

You can start editing the page by modifying `app/page.tsx`. The page auto-updates as you edit the file.

This project uses [`next/font`](https://nextjs.org/docs/app/building-your-application/optimizing/fonts) to automatically optimize and load [Geist](https://vercel.com/font), a new font family for Vercel.

## Learn More

To learn more about Next.js, take a look at the following resources:

- [Next.js Documentation](https://nextjs.org/docs) - learn about Next.js features and API.
- [Learn Next.js](https://nextjs.org/learn) - an interactive Next.js tutorial.

You can check out [the Next.js GitHub repository](https://github.com/vercel/next.js) - your feedback and contributions are welcome!

## Deploy on Vercel

The easiest way to deploy your Next.js app is to use the [Vercel Platform](https://vercel.com/new?utm_medium=default-template&filter=next.js&utm_source=create-next-app&utm_campaign=create-next-app-readme) from the creators of Next.js.

Check out our [Next.js deployment documentation](https://nextjs.org/docs/app/building-your-application/deploying) for more details.
