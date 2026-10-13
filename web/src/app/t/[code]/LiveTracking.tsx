"use client";

import { useEffect, useRef, useState } from "react";
import type { Tracking } from "@/lib/api/client";
import { OverallStamp, TrackingLine } from "@/components/TrackingLine";

// "connecting" renders nothing: claim "Live" only once the stream is open.
type Connection = "connecting" | "live" | "reconnecting";

/** Live island for the tracking page. The server already rendered the same
 *  view; this only swaps in newer ones from the event stream and announces
 *  changes politely to screen readers. */
export function LiveTracking({
  code,
  initial,
}: {
  code: string;
  initial: Tracking;
}) {
  const [tracking, setTracking] = useState(initial);
  const [connection, setConnection] = useState<Connection>("connecting");
  const [announcement, setAnnouncement] = useState("");
  const previous = useRef(initial);

  useEffect(() => {
    if (typeof EventSource === "undefined") return; // the server-rendered view stands
    const es = new EventSource(`/api/track/${encodeURIComponent(code)}/events`);
    es.addEventListener("tracking", (e) => {
      const next = JSON.parse((e as MessageEvent<string>).data) as Tracking;
      setConnection("live");
      const before = previous.current.steps.filter(
        (s) => s.status === "done",
      ).length;
      const after = next.steps.filter((s) => s.status === "done").length;
      if (after > before) {
        const latest = next.steps.filter((s) => s.status === "done").at(-1);
        setAnnouncement(
          latest ? `Update: ${latest.step.replaceAll("_", " ")} is done.` : "",
        );
      }
      previous.current = next;
      setTracking(next);
    });
    es.onopen = () => setConnection("live");
    es.onerror = () => setConnection("reconnecting"); // EventSource retries by itself
    return () => es.close();
  }, [code]);

  return (
    <>
      <div className="flex items-center justify-between gap-4">
        <OverallStamp tracking={tracking} />
        <p className="text-sm text-ink-muted" aria-hidden="true">
          {connection === "live" && "Live"}
          {connection === "reconnecting" && "Reconnecting…"}
        </p>
      </div>
      <TrackingLine tracking={tracking} />
      <p className="sr-only" aria-live="polite">
        {announcement}
      </p>
    </>
  );
}
