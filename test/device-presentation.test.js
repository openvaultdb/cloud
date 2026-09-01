import { describe, expect, it } from "vitest";

import {
  devicePresentation,
  formatDeviceDate,
  platformPresentation,
} from "../public/device-presentation.js";

describe("device presentation", () => {
  it("makes the device name primary and keeps client details secondary", () => {
    expect(
      devicePresentation({
        client: { name: "OpenVaultDB CLI" },
        device: {
          name: "Alex's MacBook Pro",
          os: "darwin",
          arch: "arm64",
          client_version: "0.2.0",
        },
      }),
    ).toEqual({
      title: "Alex's MacBook Pro",
      details: "OpenVaultDB CLI 0.2.0 · macOS · Apple silicon",
    });
  });

  it("falls back cleanly for older clients without metadata", () => {
    expect(devicePresentation({ client: { name: "OpenVaultDB CLI" } })).toEqual({
      title: "OpenVaultDB CLI",
      details: "",
    });
  });

  it("formats common platforms without inventing missing values", () => {
    expect(platformPresentation("linux", "amd64")).toBe("Linux · x86-64");
    expect(platformPresentation("", "")).toBe("");
    expect(formatDeviceDate("not-a-date")).toBe("Unknown");
    expect(formatDeviceDate(null)).toBe("Never");
  });
});
