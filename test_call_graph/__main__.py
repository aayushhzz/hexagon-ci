"""Command-line entry point for full call graph rebuilds."""

import argparse
import sys

from .rebuild import rebuild_to_file
from .go_frontend import GoFrontend


def main() -> int:
    parser = argparse.ArgumentParser(description="Rebuild per-test source call graphs")
    parser.add_argument("repository", help="Path to the repository to scan")
    parser.add_argument("--output", required=True, help="JSON file to replace after a successful rebuild")
    parser.add_argument("--language", choices=["python", "go"], default="python")
    parser.add_argument("--go-helper", help="Prebuilt Go AST helper for repeated Go rebuilds")
    parser.add_argument("--go-tags", default="", help="Comma-separated Go build tags to analyze")
    args = parser.parse_args()
    try:
        frontend = GoFrontend(args.go_helper, tags=args.go_tags.split(",") if args.go_tags else ()) if args.language == "go" else None
        graph = rebuild_to_file(args.repository, args.output, frontend)
    except (OSError, SyntaxError, UnicodeError, ValueError, RuntimeError) as error:
        print("call graph rebuild failed: {}".format(error), file=sys.stderr)
        return 1
    print("Rebuilt {} test graphs from {} {} files".format(
        len(graph["tests"]), graph["file_count"], args.language
    ))
    return 0


if __name__ == "__main__":
    sys.exit(main())
