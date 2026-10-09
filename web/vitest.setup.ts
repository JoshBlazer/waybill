import { cleanup } from "@testing-library/react";
import { afterEach } from "vitest";

// Vitest runs without globals, so Testing Library cannot register its own
// cleanup; unmount rendered trees between tests here.
afterEach(cleanup);
