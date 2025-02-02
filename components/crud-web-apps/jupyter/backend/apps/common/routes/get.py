from kubeflow.kubeflow.crud_backend import api, logging

from .. import utils
from . import bp

log = logging.getLogger(__name__)


@bp.route("/api/config/<namespace>")
def get_config(namespace):
    config = utils.load_spawner_ui_config(namespace)
    return api.success_response("config", config)


@bp.route("/api/namespaces/<namespace>/pvcs")
def get_pvcs(namespace):
    pvcs = api.list_pvcs(namespace).items
    data = [{"name": pvc.metadata.name,
             "size": pvc.spec.resources.requests["storage"],
             "mode": pvc.spec.access_modes[0]} for pvc in pvcs]

    return api.success_response("pvcs", data)


@bp.route("/api/namespaces/<namespace>/poddefaults")
def get_poddefaults(namespace):
    pod_defaults = api.list_poddefaults(namespace)

    # Return a list of (label, desc) with the pod defaults
    contents = []
    for pd in pod_defaults["items"]:
        label = list(pd["spec"]["selector"]["matchLabels"].keys())[0]
        if "desc" in pd["spec"]:
            desc = pd["spec"]["desc"]
        else:
            desc = pd["metadata"]["name"]

        contents.append({"label": label, "desc": desc})

    log.info("Found poddefaults: %s", contents)
    return api.success_response("poddefaults", contents)


@bp.route("/api/namespaces/<namespace>/notebooks")
def get_notebooks(namespace):
    index_config = utils.load_notebook_index_ui_config()
    spawner_config = utils.load_spawner_ui_config(namespace)
    notebooks = api.list_notebooks(namespace)["items"]
    contents = [utils.notebook_dict_from_k8s_obj(nb, index_config, spawner_config) for nb in notebooks]

    return api.success_response("notebooks", contents)


@bp.route("/api/gpus/<namespace>")
def get_gpu_vendors(namespace):
    """
    Return a list of GPU vendors for which at least one node has the necessary
    annotation required to schedule pods
    """
    frontend_config = utils.load_spawner_ui_config(namespace)
    if isinstance(frontend_config, list):
        frontend_config = frontend_config[0]

    gpus_value = frontend_config.get("gpus", {}).get("value", {})
    config_vendor_keys = [
        v.get("limitsKey", "") for v in gpus_value.get("vendors", [])
    ]

    # Get all of the different resources installed in all nodes
    installed_resources = set()
    nodes = api.list_nodes().items
    for node in nodes:
        installed_resources.update(node.status.capacity.keys())

    # Keep the vendors the key of which exists in at least one node
    available_vendors = installed_resources.intersection(config_vendor_keys)

    return api.success_response("vendors", list(available_vendors))
