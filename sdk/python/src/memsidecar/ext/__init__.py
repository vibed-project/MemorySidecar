"""Optional framework integrations for memsidecar.

Each submodule targets a specific agent framework and depends on that
framework being installed (an extra, e.g. ``pip install memsidecar[langgraph]``
or ``memsidecar[crewai]``). Importing :mod:`memsidecar` itself never pulls
these in.
"""
