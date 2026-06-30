import { createFileRoute } from '@tanstack/react-router'

// SSE proxy: the browser's EventSource hits this same-origin route, which pipes
// the runtime's event stream through with the service token injected
// server-side. The server-only client is dynamically imported so it never
// enters the client bundle.
export const Route = createFileRoute('/api/runs/$runId/events')({
  server: {
    handlers: {
      GET: async ({ params, request }) => {
        const { openRunEventStream } = await import('~/lib/runtime-api.server')
        const upstream = await openRunEventStream(params.runId, request.signal)
        if (!upstream.ok || !upstream.body) {
          return new Response('event stream unavailable', { status: 502 })
        }
        return new Response(upstream.body, {
          headers: {
            'Content-Type': 'text/event-stream',
            'Cache-Control': 'no-cache',
            Connection: 'keep-alive',
            'X-Accel-Buffering': 'no',
          },
        })
      },
    },
  },
})
