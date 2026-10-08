package main

import (
	"bufio"
	"bytes"
	"context"
	"controlplane/protocol"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
	syaml "sigs.k8s.io/yaml"
)

const defaultServerURL = "http://localhost:8080"

type cliOptions struct {
	server        string
	output        string
	namespace     string
	apiVersion    string
	allNamespaces bool
	files         []string
	watchTypes    []string
	watchName     string
}

type apiClient struct {
	baseURL string
	http    *http.Client
}

type kindList struct {
	Items []protocol.ResourceKind `json:"items"`
}

type resourceList struct {
	Items []protocol.Resource `json:"items"`
}

type apiResource struct {
	Name       string `json:"name" yaml:"name"`
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Methods    string `json:"methods" yaml:"methods"`
	Path       string `json:"path" yaml:"path"`
}

type manifest struct {
	resource *protocol.Resource
	kind     *protocol.ResourceKind
}

func main() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	options := &cliOptions{server: defaultAPIURL(), output: "table"}
	root := &cobra.Command{
		Use:           "gubctl",
		Short:         "Controlplane command-line client",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			switch options.output {
			case "table", "yaml", "yml", "json":
				return nil
			default:
				return fmt.Errorf("unsupported output format %q", options.output)
			}
		},
	}
	root.PersistentFlags().StringVar(&options.server, "server", options.server, "API server URL")
	root.PersistentFlags().StringVarP(&options.output, "output", "o", options.output, "Output format: table, yaml, or json")
	root.PersistentFlags().StringVarP(&options.namespace, "namespace", "n", "", "Resource namespace")
	root.PersistentFlags().StringVar(&options.apiVersion, "api-version", "", "Resource API version")
	root.PersistentFlags().BoolVarP(&options.allNamespaces, "all-namespaces", "A", false, "List or watch resources in all namespaces")

	root.AddCommand(newAPIResourcesCommand(options))
	root.AddCommand(newGetCommand(options))
	root.AddCommand(newDescribeCommand(options))
	root.AddCommand(newCreateCommand(options))
	root.AddCommand(newApplyCommand(options))
	root.AddCommand(newDeleteCommand(options))
	root.AddCommand(newWatchCommand(options))
	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print gubctl version",
		Run: func(command *cobra.Command, _ []string) {
			command.Println("gubctl dev")
		},
	})
	return root
}

func defaultAPIURL() string {
	for _, name := range []string{"gubctl_SERVER", "API_SERVER_URL"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	if address := os.Getenv("API_SERVER_ADDRESS"); address != "" {
		if strings.HasPrefix(address, ":") {
			return "http://localhost" + address
		}
		if !strings.Contains(address, "://") {
			return "http://" + address
		}
		return address
	}
	return defaultServerURL
}

func newAPIClient(server string) *apiClient {
	return &apiClient{
		baseURL: strings.TrimRight(server, "/"),
		http:    &http.Client{},
	}
}

func (client *apiClient) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, client.baseURL+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.http.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, response.StatusCode, err
	}
	return responseBody, response.StatusCode, nil
}

func (client *apiClient) requestJSON(ctx context.Context, method, endpoint string, value any) ([]byte, int, error) {
	var body []byte
	var err error
	if value != nil {
		body, err = json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
	}
	return client.request(ctx, method, endpoint, body)
}

func newAPIResourcesCommand(options *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:     "api-resources",
		Aliases: []string{"endpoints"},
		Short:   "List API endpoints and registered resource kinds",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client := newAPIClient(options.server)
			kinds, err := client.listKinds(command.Context())
			if err != nil {
				return err
			}
			return printAPIResources(command, options.output, kinds)
		},
	}
}

func newGetCommand(options *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "get RESOURCE [NAME]",
		Short: "List or get resources and kinds",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			client := newAPIClient(options.server)
			if strings.EqualFold(args[0], "namespace") || strings.EqualFold(args[0], "namespaces") {
				if len(args) != 1 {
					return errors.New("usage: gubctl get namespaces")
				}
				body, status, err := client.request(command.Context(), http.MethodGet, "/api/namespaces", nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				var namespaces protocol.NamespaceList
				if err := json.Unmarshal(body, &namespaces); err != nil {
					return err
				}
				return printNamespaces(command, options.output, namespaces.Items)
			}
			if strings.EqualFold(args[0], "all") || strings.EqualFold(args[0], "resources") {
				endpoint := "/api/resources"
				if options.namespace != "" && !options.allNamespaces {
					endpoint += namespaceQuery(options.namespace)
				}
				body, status, err := client.request(command.Context(), http.MethodGet, endpoint, nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				var resources resourceList
				if err := json.Unmarshal(body, &resources); err != nil {
					return err
				}
				return printResources(command, options.output, resources.Items)
			}
			if strings.EqualFold(args[0], "endpoints") || strings.EqualFold(args[0], "api-resources") {
				kinds, err := client.listKinds(command.Context())
				if err != nil {
					return err
				}
				return printAPIResources(command, options.output, kinds)
			}
			if isKindCollection(args[0]) {
				kinds, err := client.listKinds(command.Context())
				if err != nil {
					return err
				}
				if len(args) == 2 {
					selected, err := findKind(kinds, args[1], options.apiVersion)
					if err != nil {
						return err
					}
					return printKinds(command, options.output, []protocol.ResourceKind{selected})
				}
				return printKinds(command, options.output, kinds)
			}

			kind, err := client.resolveKind(command.Context(), args[0], options.apiVersion)
			if err != nil {
				return err
			}
			endpoint := resourceCollectionPath(kind)
			if len(args) == 2 {
				endpoint = resourcePath(kind, args[1]) + namespaceQuery(options.namespace)
				body, status, err := client.request(command.Context(), http.MethodGet, endpoint, nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				var resource protocol.Resource
				if err := json.Unmarshal(body, &resource); err != nil {
					return err
				}
				return printResources(command, options.output, []protocol.Resource{resource})
			}

			if options.namespace != "" && !options.allNamespaces {
				endpoint += namespaceQuery(options.namespace)
			}
			body, status, err := client.request(command.Context(), http.MethodGet, endpoint, nil)
			if err != nil {
				return err
			}
			if status < 200 || status >= 300 {
				return responseError(status, body)
			}
			var resources resourceList
			if err := json.Unmarshal(body, &resources); err != nil {
				return err
			}
			return printResources(command, options.output, resources.Items)
		},
	}
}

func newDescribeCommand(options *cliOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "describe RESOURCE NAME | describe RESOURCE/NAME | describe kind KIND",
		Short: "Show details for a resource or resource kind",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(command *cobra.Command, args []string) error {
			client := newAPIClient(options.server)
			if strings.EqualFold(args[0], "kind") || strings.EqualFold(args[0], "kinds") {
				if len(args) != 2 {
					return errors.New("usage: gubctl describe kind KIND")
				}
				kind, err := client.resolveKind(command.Context(), args[1], options.apiVersion)
				if err != nil {
					return err
				}
				body, status, err := client.request(command.Context(), http.MethodGet, kindPath(kind), nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				if err := json.Unmarshal(body, &kind); err != nil {
					return err
				}
				return printKindDescription(command, options.output, kind)
			}

			kindAndName := strings.SplitN(args[0], "/", 2)
			kindName := kindAndName[0]
			resourceName := ""
			if len(kindAndName) == 2 {
				if len(args) != 1 {
					return errors.New("provide a resource as KIND/NAME or as separate KIND NAME arguments")
				}
				resourceName = kindAndName[1]
			} else if len(args) == 2 {
				resourceName = args[1]
			}
			if resourceName == "" {
				return errors.New("a resource name is required; use gubctl get to list resources")
			}

			kind, err := client.resolveKind(command.Context(), kindName, options.apiVersion)
			if err != nil {
				return err
			}
			endpoint := resourcePath(kind, resourceName) + namespaceQuery(options.namespace)
			body, status, err := client.request(command.Context(), http.MethodGet, endpoint, nil)
			if err != nil {
				return err
			}
			if status < 200 || status >= 300 {
				return responseError(status, body)
			}
			var resource protocol.Resource
			if err := json.Unmarshal(body, &resource); err != nil {
				return err
			}
			return printResourceDescription(command, options.output, resource)
		},
	}
}

func newCreateCommand(options *cliOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "create -f FILENAME",
		Short: "Create resources or kinds from YAML definitions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if len(options.files) == 0 {
				return errors.New("at least one -f/--filename is required")
			}
			client := newAPIClient(options.server)
			for _, file := range options.files {
				definitions, err := readManifests(file)
				if err != nil {
					return err
				}
				for _, definition := range definitions {
					value, err := client.createDefinition(command.Context(), definition)
					if err != nil {
						return err
					}
					if err := printDefinitionResult(command, options.output, "created", definition, value); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	command.Flags().StringArrayVarP(&options.files, "filename", "f", nil, "YAML manifest file (repeatable; use - for stdin)")
	return command
}

func newApplyCommand(options *cliOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "apply -f FILENAME",
		Short: "Create or update resources and kinds from YAML definitions",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if len(options.files) == 0 {
				return errors.New("at least one -f/--filename is required")
			}
			client := newAPIClient(options.server)
			for _, file := range options.files {
				definitions, err := readManifests(file)
				if err != nil {
					return err
				}
				for _, definition := range definitions {
					value, err := client.applyDefinition(command.Context(), definition)
					if err != nil {
						return err
					}
					if err := printDefinitionResult(command, options.output, "configured", definition, value); err != nil {
						return err
					}
				}
			}
			return nil
		},
	}
	command.Flags().StringArrayVarP(&options.files, "filename", "f", nil, "YAML manifest file (repeatable; use - for stdin)")
	return command
}

func newDeleteCommand(options *cliOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "delete RESOURCE NAME... | delete kind KIND",
		Short: "Delete resources or resource kinds",
		Args: func(command *cobra.Command, args []string) error {
			if len(options.files) > 0 {
				if len(args) > 0 {
					return errors.New("delete accepts either resource arguments or -f/--filename, not both")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(command, args)
		},
		RunE: func(command *cobra.Command, args []string) error {
			client := newAPIClient(options.server)
			if len(options.files) > 0 {
				if len(args) > 0 {
					return errors.New("delete accepts either resource arguments or -f/--filename, not both")
				}
				for _, file := range options.files {
					definitions, err := readManifests(file)
					if err != nil {
						return err
					}
					for _, definition := range definitions {
						if err := client.deleteDefinition(command.Context(), definition); err != nil {
							return err
						}
					}
				}
				return nil
			}
			if args[0] == "kind" || args[0] == "kinds" {
				if len(args) != 2 {
					return errors.New("usage: gubctl delete kind KIND")
				}
				kind, err := client.resolveKind(command.Context(), args[1], options.apiVersion)
				if err != nil {
					return err
				}
				body, status, err := client.request(command.Context(), http.MethodDelete, kindPath(kind), nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				command.Printf("deleted kind %s/%s\n", kind.APIVersion, kind.Kind)
				return nil
			}

			kind, err := client.resolveKind(command.Context(), args[0], options.apiVersion)
			if err != nil {
				return err
			}
			for _, name := range args[1:] {
				body, status, err := client.request(command.Context(), http.MethodDelete, resourcePath(kind, name)+namespaceQuery(options.namespace), nil)
				if err != nil {
					return err
				}
				if status < 200 || status >= 300 {
					return responseError(status, body)
				}
				command.Printf("deleted %s/%s\n", kind.Kind, name)
			}
			return nil
		},
	}
	command.Flags().StringArrayVarP(&options.files, "filename", "f", nil, "YAML manifest file (repeatable)")
	return command
}

func newWatchCommand(options *cliOptions) *cobra.Command {
	command := &cobra.Command{
		Use:   "watch [RESOURCE[/NAME]]",
		Short: "Watch all events or filter by kind, resource, namespace, and event type",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client := newAPIClient(options.server)
			kindName := ""
			name := options.watchName
			if len(args) == 1 {
				parts := strings.SplitN(args[0], "/", 2)
				kindName = parts[0]
				if len(parts) == 2 {
					name = parts[1]
				}
			}

			apiVersion := options.apiVersion
			if kindName != "" {
				kind, err := client.resolveKind(command.Context(), kindName, apiVersion)
				if err != nil {
					return err
				}
				kindName = kind.Kind
				apiVersion = kind.APIVersion
			}

			query := url.Values{}
			if apiVersion != "" {
				query.Set("apiVersion", apiVersion)
			}
			if kindName != "" {
				query.Set("kind", kindName)
			}
			if options.namespace != "" && !options.allNamespaces {
				query.Set("namespace", options.namespace)
			}
			endpoint := "/api/watch"
			if encoded := query.Encode(); encoded != "" {
				endpoint += "?" + encoded
			}
			request, err := http.NewRequestWithContext(command.Context(), http.MethodGet, client.baseURL+endpoint, nil)
			if err != nil {
				return err
			}
			response, err := client.http.Do(request)
			if err != nil {
				return err
			}
			defer response.Body.Close()
			if response.StatusCode < 200 || response.StatusCode >= 300 {
				body, _ := io.ReadAll(response.Body)
				return responseError(response.StatusCode, body)
			}
			filterNamespace := options.namespace
			if options.allNamespaces {
				filterNamespace = ""
			}
			return consumeWatch(command, options.output, response.Body, kindName, name, filterNamespace, options.watchTypes)
		},
	}
	command.Flags().StringVar(&options.watchName, "name", "", "Filter events by resource name")
	command.Flags().StringArrayVar(&options.watchTypes, "type", nil, "Event type to display: ADDED, MODIFIED, or DELETED (repeatable)")
	return command
}

func (client *apiClient) listKinds(ctx context.Context) ([]protocol.ResourceKind, error) {
	body, status, err := client.request(ctx, http.MethodGet, "/api/kinds", nil)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, responseError(status, body)
	}
	var result kindList
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (client *apiClient) resolveKind(ctx context.Context, value, apiVersion string) (protocol.ResourceKind, error) {
	kinds, err := client.listKinds(ctx)
	if err != nil {
		return protocol.ResourceKind{}, err
	}
	var matches []protocol.ResourceKind
	for _, kind := range kinds {
		if apiVersion != "" && kind.APIVersion != apiVersion {
			continue
		}
		if strings.EqualFold(kind.Kind, value) || strings.EqualFold(kind.Resource, value) {
			matches = append(matches, kind)
		}
	}
	if len(matches) == 0 {
		return protocol.ResourceKind{}, fmt.Errorf("resource kind %q is not registered", value)
	}
	if len(matches) > 1 {
		return protocol.ResourceKind{}, fmt.Errorf("resource kind %q has multiple API versions; specify --api-version", value)
	}
	return matches[0], nil
}

func (client *apiClient) createDefinition(ctx context.Context, definition manifest) (any, error) {
	if definition.kind != nil {
		body, status, err := client.requestJSON(ctx, http.MethodPost, "/api/kinds", definition.kind)
		return decodeResponse(body, status, err)
	}
	resource := definition.resource
	body, status, err := client.requestJSON(ctx, http.MethodPost, resourceCollectionPath(protocol.ResourceKind{APIVersion: resource.APIVersion, Kind: resource.Kind}), resource)
	return decodeResponse(body, status, err)
}

func (client *apiClient) applyDefinition(ctx context.Context, definition manifest) (any, error) {
	if definition.kind != nil {
		kind := definition.kind
		endpoint := kindPath(*kind)
		_, status, err := client.request(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		method := http.MethodPut
		if status == http.StatusNotFound {
			method = http.MethodPost
			endpoint = "/api/kinds"
		} else if status < 200 || status >= 300 {
			return nil, fmt.Errorf("get kind: %s", http.StatusText(status))
		}
		body, resultStatus, err := client.requestJSON(ctx, method, endpoint, kind)
		return decodeResponse(body, resultStatus, err)
	}

	resource := definition.resource
	kind := protocol.ResourceKind{APIVersion: resource.APIVersion, Kind: resource.Kind}
	endpoint := resourcePath(kind, resource.Metadata.Name) + namespaceQuery(resource.Metadata.Namespace)
	_, status, err := client.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	method := http.MethodPut
	if status == http.StatusNotFound {
		method = http.MethodPost
		endpoint = resourceCollectionPath(kind)
	} else if status < 200 || status >= 300 {
		return nil, fmt.Errorf("get resource: %s", http.StatusText(status))
	}
	body, resultStatus, err := client.requestJSON(ctx, method, endpoint, resource)
	return decodeResponse(body, resultStatus, err)
}

func (client *apiClient) deleteDefinition(ctx context.Context, definition manifest) error {
	var endpoint string
	if definition.kind != nil {
		endpoint = kindPath(*definition.kind)
	} else {
		resource := definition.resource
		endpoint = resourcePath(protocol.ResourceKind{APIVersion: resource.APIVersion, Kind: resource.Kind}, resource.Metadata.Name) + namespaceQuery(resource.Metadata.Namespace)
	}
	body, status, err := client.request(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return responseError(status, body)
	}
	return nil
}

func decodeResponse(body []byte, status int, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, responseError(status, body)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func readManifests(filename string) ([]manifest, error) {
	var reader io.Reader
	if filename == "-" {
		reader = os.Stdin
	} else {
		file, err := os.Open(filename)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		reader = file
	}

	decoder := yamlv3.NewDecoder(reader)
	var definitions []manifest
	for documentNumber := 1; ; documentNumber++ {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s document %d: %w", filename, documentNumber, err)
		}
		if len(document) == 0 {
			continue
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			return nil, err
		}
		if _, isKind := fields["resource"]; isKind {
			var kind protocol.ResourceKind
			if err := json.Unmarshal(encoded, &kind); err != nil {
				return nil, err
			}
			if kind.APIVersion == "" || kind.Kind == "" || kind.Resource == "" {
				return nil, fmt.Errorf("%s document %d: kind requires apiVersion, kind, and resource", filename, documentNumber)
			}
			definitions = append(definitions, manifest{kind: &kind})
			continue
		}
		var resource protocol.Resource
		if err := json.Unmarshal(encoded, &resource); err != nil {
			return nil, err
		}
		if resource.APIVersion == "" || resource.Kind == "" || resource.Metadata.Name == "" {
			return nil, fmt.Errorf("%s document %d: resource requires apiVersion, kind, and metadata.name", filename, documentNumber)
		}
		definitions = append(definitions, manifest{resource: &resource})
	}
	if len(definitions) == 0 {
		return nil, fmt.Errorf("%s contains no definitions", filename)
	}
	return definitions, nil
}

func printAPIResources(command *cobra.Command, output string, kinds []protocol.ResourceKind) error {
	resources := []apiResource{
		{Name: "kinds", APIVersion: "v1", Kind: "ResourceKind", Methods: "GET,POST", Path: "/api/kinds"},
		{Name: "kind detail", APIVersion: "v1", Kind: "ResourceKind", Methods: "GET,PUT,DELETE", Path: "/api/kinds/{apiVersion}/{kind}"},
		{Name: "resources", APIVersion: "v1", Kind: "ResourceList", Methods: "GET", Path: "/api/resources"},
		{Name: "namespaces", APIVersion: "v1", Kind: "NamespaceList", Methods: "GET", Path: "/api/namespaces"},
		{Name: "watch", APIVersion: "v1", Kind: "WatchEvent", Methods: "GET,PUT", Path: "/api/watch"},
	}
	for _, kind := range kinds {
		resources = append(resources,
			apiResource{Name: kind.Resource, APIVersion: kind.APIVersion, Kind: kind.Kind, Methods: "GET,POST", Path: resourceCollectionPath(kind)},
			apiResource{Name: kind.Resource + "/" + kind.Kind, APIVersion: kind.APIVersion, Kind: kind.Kind, Methods: "GET,PUT,DELETE", Path: resourceCollectionPath(kind) + "/{name}"},
		)
	}
	if output != "table" {
		return printValue(command, output, resources)
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tAPIVERSION\tKIND\tMETHODS\tPATH")
	for _, resource := range resources {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", resource.Name, resource.APIVersion, resource.Kind, resource.Methods, resource.Path)
	}
	return writer.Flush()
}

func printDefinitionResult(command *cobra.Command, output, verb string, definition manifest, value any) error {
	if output != "table" {
		return printValue(command, output, value)
	}
	if definition.kind != nil {
		command.Printf("%s kind/%s (%s)\n", verb, definition.kind.Kind, definition.kind.APIVersion)
	} else {
		command.Printf("%s %s/%s\n", verb, definition.resource.Kind, definition.resource.Metadata.Name)
	}
	return nil
}

func printKinds(command *cobra.Command, output string, kinds []protocol.ResourceKind) error {
	if output != "table" {
		return printValue(command, output, kinds)
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "KIND\tAPIVERSION\tRESOURCE\tNAMESPACED")
	for _, kind := range kinds {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%t\n", kind.Kind, kind.APIVersion, kind.Resource, kind.Namespaced)
	}
	return writer.Flush()
}

func printNamespaces(command *cobra.Command, output string, namespaces []protocol.Namespace) error {
	if output != "table" {
		return printValue(command, output, namespaces)
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME")
	for _, namespace := range namespaces {
		fmt.Fprintln(writer, namespace.Name)
	}
	return writer.Flush()
}

func printResources(command *cobra.Command, output string, resources []protocol.Resource) error {
	if output != "table" {
		return printValue(command, output, resources)
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "NAME\tNAMESPACE\tKIND\tAPIVERSION\tGENERATION")
	for _, resource := range resources {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\n", resource.Metadata.Name, resource.Metadata.Namespace, resource.Kind, resource.APIVersion, resource.Metadata.Generation)
	}
	return writer.Flush()
}

func printKindDescription(command *cobra.Command, output string, kind protocol.ResourceKind) error {
	if output != "table" {
		return printValue(command, output, kind)
	}
	writer := command.OutOrStdout()
	fmt.Fprintf(writer, "Name:         %s\n", kind.Kind)
	fmt.Fprintf(writer, "API Version:  %s\n", kind.APIVersion)
	fmt.Fprintf(writer, "Resource:     %s\n", kind.Resource)
	fmt.Fprintf(writer, "Namespaced:   %t\n", kind.Namespaced)
	fmt.Fprintln(writer, "Schema:")
	if len(kind.Schema) == 0 {
		_, err := fmt.Fprintln(writer, "  <none>")
		return err
	}
	return writeIndentedYAML(writer, kind.Schema, "  ")
}

func printResourceDescription(command *cobra.Command, output string, resource protocol.Resource) error {
	if output != "table" {
		return printValue(command, output, resource)
	}
	writer := command.OutOrStdout()
	fmt.Fprintf(writer, "Name:            %s\n", resource.Metadata.Name)
	fmt.Fprintf(writer, "Kind:            %s\n", resource.Kind)
	fmt.Fprintf(writer, "API Version:     %s\n", resource.APIVersion)
	writeDescriptionValue(writer, "Namespace", resource.Metadata.Namespace)
	writeDescriptionValue(writer, "UID", resource.Metadata.UID)
	fmt.Fprintf(writer, "Generation:      %d\n", resource.Metadata.Generation)
	fmt.Fprintf(writer, "Resource Version: %d\n", resource.Metadata.ResourceVersion)
	fmt.Fprintln(writer, "Labels:")
	if len(resource.Metadata.Labels) == 0 {
		fmt.Fprintln(writer, "  <none>")
	} else if err := writeIndentedYAML(writer, resource.Metadata.Labels, "  "); err != nil {
		return err
	}
	fmt.Fprintln(writer, "Annotations:")
	if len(resource.Metadata.Annotations) == 0 {
		fmt.Fprintln(writer, "  <none>")
	} else if err := writeIndentedYAML(writer, resource.Metadata.Annotations, "  "); err != nil {
		return err
	}
	fmt.Fprintln(writer, "Spec:")
	if len(resource.Spec) == 0 {
		fmt.Fprintln(writer, "  <none>")
	} else if err := writeIndentedYAML(writer, resource.Spec, "  "); err != nil {
		return err
	}
	fmt.Fprintln(writer, "Status:")
	if len(resource.Status) == 0 {
		fmt.Fprintln(writer, "  <none>")
		return nil
	}
	return writeIndentedYAML(writer, resource.Status, "  ")
}

func writeDescriptionValue(writer io.Writer, label, value string) {
	if value == "" {
		value = "<none>"
	}
	fmt.Fprintf(writer, "%-16s %s\n", label+":", value)
}

func writeIndentedYAML(writer io.Writer, value any, indent string) error {
	data, err := yamlv3.Marshal(value)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if _, err := fmt.Fprintf(writer, "%s%s\n", indent, line); err != nil {
			return err
		}
	}
	return nil
}

func printValue(command *cobra.Command, output string, value any) error {
	var data []byte
	var err error
	switch output {
	case "json":
		data, err = json.MarshalIndent(value, "", "  ")
	case "yaml", "yml":
		var jsonData []byte
		jsonData, err = json.Marshal(value)
		if err == nil {
			data, err = syaml.JSONToYAML(jsonData)
		}
	case "table":
		data, err = json.MarshalIndent(value, "", "  ")
	default:
		return fmt.Errorf("unsupported output format %q", output)
	}
	if err != nil {
		return err
	}
	_, err = command.OutOrStdout().Write(append(data, '\n'))
	return err
}

func consumeWatch(command *cobra.Command, output string, reader io.Reader, kind, name, namespace string, types []string) error {
	allowedTypes := make(map[protocol.EventType]bool)
	for _, value := range types {
		for _, eventType := range strings.Split(value, ",") {
			switch protocol.EventType(strings.ToUpper(strings.TrimSpace(eventType))) {
			case protocol.Added, protocol.Modified, protocol.Deleted:
				allowedTypes[protocol.EventType(strings.ToUpper(strings.TrimSpace(eventType)))] = true
			default:
				return fmt.Errorf("unknown event type %q", eventType)
			}
		}
	}

	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	if output == "table" {
		fmt.Fprintln(writer, "TYPE\tKIND\tNAME\tNAMESPACE\tRESOURCEVERSION")
		if err := writer.Flush(); err != nil {
			return err
		}
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event protocol.WatchEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return fmt.Errorf("decode watch event: %w", err)
		}
		resource := event.Object
		if kind != "" && resource.Kind != kind {
			continue
		}
		if name != "" && resource.Metadata.Name != name {
			continue
		}
		if namespace != "" && resource.Metadata.Namespace != namespace {
			continue
		}
		if len(allowedTypes) > 0 && !allowedTypes[event.Type] {
			continue
		}
		if output == "table" {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\n", event.Type, resource.Kind, resource.Metadata.Name, resource.Metadata.Namespace, resource.Metadata.ResourceVersion)
			if err := writer.Flush(); err != nil {
				return err
			}
			continue
		}
		if output == "json" {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), string(encoded)); err != nil {
				return err
			}
			continue
		}
		if output == "yaml" || output == "yml" {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			formatted, err := syaml.JSONToYAML(encoded)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(command.OutOrStdout(), "---"); err != nil {
				return err
			}
			if _, err := command.OutOrStdout().Write(formatted); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("unsupported output format %q", output)
	}
	return scanner.Err()
}

func isKindCollection(value string) bool {
	return strings.EqualFold(value, "kind") || strings.EqualFold(value, "kinds")
}

func kindPath(kind protocol.ResourceKind) string {
	return "/api/kinds/" + url.PathEscape(kind.APIVersion) + "/" + url.PathEscape(kind.Kind)
}

func resourceCollectionPath(kind protocol.ResourceKind) string {
	return "/api/" + pathSegment(kind.APIVersion) + "/" + pathSegment(kind.Kind)
}

func resourcePath(kind protocol.ResourceKind, name string) string {
	return resourceCollectionPath(kind) + "/" + url.PathEscape(name)
}

func pathSegment(value string) string {
	return url.PathEscape(value)
}

func findKind(kinds []protocol.ResourceKind, value, apiVersion string) (protocol.ResourceKind, error) {
	var matches []protocol.ResourceKind
	for _, kind := range kinds {
		if apiVersion != "" && kind.APIVersion != apiVersion {
			continue
		}
		if strings.EqualFold(kind.Kind, value) || strings.EqualFold(kind.Resource, value) {
			matches = append(matches, kind)
		}
	}
	if len(matches) == 0 {
		return protocol.ResourceKind{}, fmt.Errorf("resource kind %q is not registered", value)
	}
	if len(matches) > 1 {
		return protocol.ResourceKind{}, fmt.Errorf("resource kind %q has multiple API versions; specify --api-version", value)
	}
	return matches[0], nil
}

func namespaceQuery(namespace string) string {
	if namespace == "" {
		return ""
	}
	return "?namespace=" + url.QueryEscape(namespace)
}

func responseError(status int, body []byte) error {
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("API request failed (%d): %s", status, message)
}
