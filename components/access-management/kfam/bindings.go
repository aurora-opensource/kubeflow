// Copyright 2019 The Kubeflow Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kfam

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	istioSecurity "istio.io/api/security/v1beta1"
	istioSecurityClient "istio.io/client-go/pkg/apis/security/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1 "k8s.io/client-go/listers/rbac/v1"
	"k8s.io/client-go/rest"
)

const AuthorizationPolicy = "authorizationpolicies"
const USER = "user"
const ROLE = "role"

//roleBindingNameMap maps frontend role names to k8s role names and vice-versa
var roleBindingNameMap = map[string]string{
	"kubeflow-admin": "admin",
	"kubeflow-edit":  "edit",
	"kubeflow-view":  "view",
	"admin":          "kubeflow-admin",
	"edit":           "kubeflow-edit",
	"view":           "kubeflow-view",
}

type BindingInterface interface {
	Create(binding *Binding, userIdHeader string, userIdPrefix string) error
	Delete(binding *Binding) error
	List(user string, namespaces []string, role string) (*BindingEntries, error)
}

type BindingClient struct {
	restClient        rest.Interface
	kubeClient        *clientset.Clientset
	roleBindingLister v1.RoleBindingLister
}

//getBindingName returns bindingName, which is combination of user kind, username, RoleRef kind, RoleRef name.
func getBindingName(binding *Binding) (string, error) {
	// Only keep lower case letters and numbers, replace other with -
	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return "", err
	}
	nameRaw := strings.ToLower(
		strings.Join([]string{
			binding.User.Kind,
			url.QueryEscape(reg.ReplaceAllString(binding.User.Name, "-")),
			binding.RoleRef.Kind,
			binding.RoleRef.Name,
		}, "-"),
	)

	return reg.ReplaceAllString(nameRaw, "-"), nil
}

func getAuthorizationPolicy(binding *Binding, userIdHeader string, userIdPrefix string) istioSecurity.AuthorizationPolicy {
	return istioSecurity.AuthorizationPolicy{
		Rules: []*istioSecurity.Rule{
			{
				When: []*istioSecurity.Condition{
					{
						Key: fmt.Sprintf("request.headers[%v]", userIdHeader),
						Values: []string{
							userIdPrefix + binding.User.Name,
						},
					},
				},
			},
		},
	}
}

func (c *BindingClient) Create(binding *Binding, userIdHeader string, userIdPrefix string) error {
	// TODO: permission check before go ahead
	bindingName, err := getBindingName(binding)
	if err != nil {
		return err
	}
	roleBinding := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{USER: binding.User.Name, ROLE: binding.RoleRef.Name},
			Name:        bindingName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: binding.RoleRef.APIGroup,
			Kind:     binding.RoleRef.Kind,
			Name:     roleBindingNameMap[binding.RoleRef.Name],
		},
		Subjects: []rbacv1.Subject{
			*binding.User,
		},
	}
	_, err = c.kubeClient.RbacV1().RoleBindings(binding.ReferredNamespace).Create(&roleBinding)
	if err != nil {
		return err
	}

	// create istio authorization policy
	istioAuth := &istioSecurityClient.AuthorizationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{USER: binding.User.Name, ROLE: binding.RoleRef.Name},
			Name:        bindingName,
			Namespace:   binding.ReferredNamespace,
		},
		Spec: getAuthorizationPolicy(binding, userIdHeader, userIdPrefix),
	}

	result := istioSecurityClient.AuthorizationPolicy{}
	return c.restClient.
		Post().
		Namespace(binding.ReferredNamespace).
		Resource(AuthorizationPolicy).
		Body(istioAuth).
		Do().
		Into(&result)
}

func (c *BindingClient) Delete(binding *Binding) error {
	// TODO: permission check before go ahead
	// First check existence
	bindingName, err := getBindingName(binding)
	if err != nil {
		return err
	}
	_, err = c.roleBindingLister.RoleBindings(binding.ReferredNamespace).Get(bindingName)
	if err != nil {
		return err
	}
	result := istioSecurityClient.AuthorizationPolicy{}
	err = c.restClient.
		Get().
		Namespace(binding.ReferredNamespace).
		Resource(AuthorizationPolicy).
		Name(bindingName).
		VersionedParams(&metav1.GetOptions{}, scheme.ParameterCodec).
		Do().
		Into(&result)
	if err != nil {
		return err
	}
	// Delete if exists
	err = c.kubeClient.RbacV1().RoleBindings(binding.ReferredNamespace).Delete(bindingName, &metav1.DeleteOptions{})
	if err != nil {
		return err
	}
	return c.restClient.
		Delete().
		Namespace(binding.ReferredNamespace).
		Resource(AuthorizationPolicy).
		Name(bindingName).
		Body(&metav1.DeleteOptions{}).
		Do().
		Error()
}

func (c *BindingClient) List(user string, namespaces []string, role string) (*BindingEntries, error) {
	entries := &BindingEntries{}
	isDuplicate := map[string]bool{}
	for _, ns := range namespaces {
		roleBindings, err := c.roleBindingLister.RoleBindings(ns).List(labels.Everything())
		if err != nil {
			fmt.Printf("failed listing rolebindings for namespace %s: %s\n", ns, err)
			return nil, err
		}
		for _, roleBinding := range roleBindings {
			mappedName, validName := roleBindingNameMap[roleBinding.RoleRef.Name]
			if !validName {
				fmt.Printf("skipping rolebinding %s:%s for invalid roleref name %s\n",
					ns,
					roleBinding.Name,
					roleBinding.RoleRef.Name)
				continue
			}

			if role != "" && mappedName != role && roleBinding.RoleRef.Name != role { // either is considered a match
				fmt.Printf("skipping rolebinding %s:%s for roleref name %s not matching %s\n",
					ns,
					roleBinding.Name,
					roleBinding.RoleRef.Name,
					role)
				continue
			}

			for _, sub := range roleBinding.Subjects {
				if sub.Kind != "User" || sub.APIGroup != "rbac.authorization.k8s.io" || sub.Name != user {
					fmt.Printf("skipping rolebinding %s:%s for subject %s:%s:%s not matching rbac.authorization.k8s.io:User:%s\n",
						ns,
						roleBinding.Name,
						sub.APIGroup,
						sub.Kind,
						sub.Name,
						user)
					continue
				}

				fmt.Printf("found binding entry match %s:%s for user %s, role %s\n", ns, roleBinding.Name, user, role)
				binding := Binding{
					User:              &sub,
					ReferredNamespace: ns,
					RoleRef: &rbacv1.RoleRef{
						Kind: roleBinding.RoleRef.Kind,
						Name: roleBindingNameMap[roleBinding.RoleRef.Name],
					},
				}

				bindingJSONBytes, err := json.Marshal(binding)
				if err != nil {
					return nil, fmt.Errorf("failed marshalling binding %s:%s to json: %s", ns, roleBinding.Name, err)
				}

				bindingJSON := string(bindingJSONBytes)
				if !isDuplicate[bindingJSON] {
					entries.Bindings = append(entries.Bindings, binding)
					isDuplicate[bindingJSON] = true
				}
			}
		}
	}

	fmt.Printf("total of %d binding entries found for user %s and role %s\n", len(entries.Bindings), user, role)
	return entries, nil
}
