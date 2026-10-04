#!/usr/bin/env python3
"""Expose Chromium's loopback-only CDP listener inside its microVM."""

import argparse
import asyncio
import contextlib


async def copy(source: asyncio.StreamReader, destination: asyncio.StreamWriter) -> None:
    try:
        while chunk := await source.read(64 * 1024):
            destination.write(chunk)
            await destination.drain()
    finally:
        with contextlib.suppress(BrokenPipeError, ConnectionResetError):
            destination.write_eof()


async def handle(
    client_reader: asyncio.StreamReader,
    client_writer: asyncio.StreamWriter,
    target_port: int,
) -> None:
    try:
        server_reader, server_writer = await asyncio.open_connection(
            "127.0.0.1", target_port
        )
    except OSError:
        client_writer.close()
        await client_writer.wait_closed()
        return

    try:
        await asyncio.gather(
            copy(client_reader, server_writer),
            copy(server_reader, client_writer),
        )
    finally:
        server_writer.close()
        client_writer.close()
        await asyncio.gather(
            server_writer.wait_closed(),
            client_writer.wait_closed(),
            return_exceptions=True,
        )


async def main(listen_port: int, target_port: int) -> None:
    server = await asyncio.start_server(
        lambda reader, writer: handle(reader, writer, target_port),
        "0.0.0.0",
        listen_port,
    )
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen-port", required=True, type=int)
    parser.add_argument("--target-port", required=True, type=int)
    arguments = parser.parse_args()
    asyncio.run(main(arguments.listen_port, arguments.target_port))
